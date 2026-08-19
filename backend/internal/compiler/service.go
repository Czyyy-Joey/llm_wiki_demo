package compiler

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/indexing"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
)

const (
	RunPending       = "pending"
	RunAnalyzed      = "analyzed"
	RunPlanned       = "planned"
	RunValidated     = "validated"
	RunApplied       = "applied"
	RunRenderPending = "render_pending"
	RunFailed        = "failed"
)

type Service struct {
	DB        *sql.DB
	DataRoot  string
	LLM       llm.LLMClient
	Index     *indexing.Service
	Render    func(string, []domain.WikiPage, []domain.WikiClaim, []domain.ClaimEvidence) error
	FailAfter int // 0 disables injection; a positive value fails before that action index.
}

type Result struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

type Run struct {
	ID            string          `json:"id"`
	DocumentID    string          `json:"document_id"`
	Status        string          `json:"status"`
	Analyze       json.RawMessage `json:"analyze,omitempty"`
	Candidates    json.RawMessage `json:"candidates,omitempty"`
	Plan          json.RawMessage `json:"plan,omitempty"`
	Validation    json.RawMessage `json:"validation,omitempty"`
	ApplyResult   json.RawMessage `json:"apply_result,omitempty"`
	Diff          json.RawMessage `json:"diff,omitempty"`
	Model         string          `json:"model,omitempty"`
	PromptVersion string          `json:"prompt_version,omitempty"`
	Error         string          `json:"error,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

func (s Service) Compile(ctx context.Context, documentID string) (Result, error) {
	doc, err := s.loadDocument(ctx, documentID)
	if err != nil {
		return Result{}, err
	}
	chunks, err := s.loadChunks(ctx, documentID)
	if err != nil {
		return Result{}, err
	}
	if doc.Status != "parsed" || len(chunks) == 0 {
		return Result{}, fmt.Errorf("source %s is not parsed", documentID)
	}
	runID := newID("run", documentID+time.Now().UTC().Format(time.RFC3339Nano))
	created := time.Now().UTC()
	client := s.LLM
	if client == nil {
		return Result{}, errors.New("compiler LLM is not configured; configure COMPILER_LLM_* or explicitly enable the development fake")
	}
	model := "configured-client"
	if named, ok := client.(interface{ Name() string }); ok {
		model = named.Name()
	}
	if _, err = s.DB.ExecContext(ctx, `INSERT INTO compilation_runs (id, document_id, status, model, prompt_version, created_at) VALUES (?, ?, ?, ?, ?, ?)`, runID, documentID, RunPending, model, "phase2-v2", created.Format(time.RFC3339Nano)); err != nil {
		return Result{}, err
	}
	fail := func(cause error) (Result, error) {
		_, _ = s.DB.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, error = ? WHERE id = ?`, RunFailed, cause.Error(), runID)
		return Result{RunID: runID, Status: RunFailed}, cause
	}
	analyzeRaw, err := client.Analyze(ctx, llm.AnalyzeInput{Document: doc, Chunks: chunks})
	if err != nil {
		return fail(err)
	}
	analysis, err := llm.DecodeStrict[domain.SourceAnalysis](analyzeRaw)
	if err != nil || analysis.DocumentID != documentID {
		if err == nil {
			err = errors.New("analysis document_id mismatch")
		}
		return fail(err)
	}
	if err = validateAnalysis(analysis, chunks); err != nil {
		return fail(err)
	}
	if _, err = s.DB.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, analyze_json = ? WHERE id = ?`, RunAnalyzed, string(analyzeRaw), runID); err != nil {
		return fail(err)
	}
	candidates, err := s.match(ctx, analysis)
	if err != nil {
		return fail(err)
	}
	candidatesRaw, _ := json.Marshal(candidates)
	if _, err = s.DB.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, candidates_json = ? WHERE id = ?`, RunPlanned, string(candidatesRaw), runID); err != nil {
		return fail(err)
	}
	planRaw, err := client.Plan(ctx, llm.PlanInput{DocumentID: documentID, Analysis: analyzeRaw, Candidates: candidatesRaw})
	if err != nil {
		return fail(err)
	}
	plan, err := llm.DecodeStrict[domain.CompilationPlan](planRaw)
	if err != nil || plan.DocumentID != documentID {
		if err == nil {
			err = errors.New("plan document_id mismatch")
		}
		return fail(err)
	}
	if err = s.ValidatePlan(ctx, plan, documentID); err != nil {
		return failWithPlan(ctx, s.DB, runID, planRaw, err)
	}
	if _, err = s.DB.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, plan_json = ?, validation_json = ? WHERE id = ?`, RunValidated, string(planRaw), `{"valid":true}`, runID); err != nil {
		return fail(err)
	}
	diff, err := s.ApplyPlan(ctx, runID, plan)
	if err != nil {
		var renderErr *RenderPendingError
		if errors.As(err, &renderErr) {
			return Result{RunID: runID, Status: RunRenderPending}, nil
		}
		return failWithPlan(ctx, s.DB, runID, planRaw, err)
	}
	diffRaw, _ := json.Marshal(diff)
	if s.Index != nil {
		if _, indexErr := s.Index.Reindex(ctx); indexErr != nil {
			_, _ = s.DB.ExecContext(ctx, `UPDATE wiki_pages SET index_status = 'index_pending' WHERE status = 'active'`)
		} else {
			_, _ = s.DB.ExecContext(ctx, `UPDATE wiki_pages SET index_status = 'clean' WHERE status = 'active'`)
		}
	}
	if _, err = s.DB.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, apply_result_json = ?, diff_json = ? WHERE id = ?`, RunApplied, `{"applied":true}`, string(diffRaw), runID); err != nil {
		// ApplyPlan has already committed the Wiki transaction and marked the run
		// render_pending. Keep that recoverable state instead of reporting a
		// failed run after the Wiki has changed.
		return Result{RunID: runID, Status: RunRenderPending}, err
	}
	return Result{RunID: runID, Status: RunApplied}, nil
}

type RenderPendingError struct{ Cause error }

func (e *RenderPendingError) Error() string { return "render wiki projection: " + e.Cause.Error() }
func (e *RenderPendingError) Unwrap() error { return e.Cause }

func (s Service) RetryRender(ctx context.Context, runID string) error {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != RunRenderPending {
		return fmt.Errorf("compilation run %s is not render_pending", runID)
	}
	render := s.Render
	if render == nil {
		if err := s.renderDefaultProjection(ctx); err != nil {
			return err
		}
	} else {
		pages, claims, evidence, err := s.loadWikiState(ctx)
		if err != nil {
			return &RenderPendingError{Cause: err}
		}
		if err := render(s.DataRoot, pages, claims, evidence); err != nil {
			return &RenderPendingError{Cause: err}
		}
	}
	if s.Index != nil {
		if _, err := s.Index.Reindex(ctx); err != nil {
			return fmt.Errorf("rebuild retrieval indexes: %w", err)
		}
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, apply_result_json = ?, error = NULL WHERE id = ?`, RunApplied, `{"applied":true,"rendered":true}`, runID)
	return err
}

func failWithPlan(ctx context.Context, db *sql.DB, runID string, plan json.RawMessage, cause error) (Result, error) {
	validation, _ := json.Marshal(map[string]any{"valid": false, "error": cause.Error()})
	_, _ = db.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, plan_json = ?, validation_json = ?, error = ? WHERE id = ?`, RunFailed, string(plan), string(validation), cause.Error(), runID)
	return Result{RunID: runID, Status: RunFailed}, cause
}

func (s Service) GetRun(ctx context.Context, id string) (Run, error) {
	var run Run
	err := scanRun(s.DB.QueryRowContext(ctx, `SELECT id, document_id, status, analyze_json, candidates_json, plan_json, validation_json, apply_result_json, diff_json, model, prompt_version, error, created_at FROM compilation_runs WHERE id = ?`, id), &run)
	return run, err
}

func (s Service) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, document_id, status, analyze_json, candidates_json, plan_json, validation_json, apply_result_json, diff_json, model, prompt_version, error, created_at FROM compilation_runs ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Run{}
	for rows.Next() {
		var run Run
		if err := scanRun(rows, &run); err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return result, rows.Err()
}

type runScanner interface{ Scan(...any) error }

func scanRun(row runScanner, run *Run) error {
	var created, analyze, candidates, plan, validation, applyResult, diff, model, prompt, runError sql.NullString
	err := row.Scan(&run.ID, &run.DocumentID, &run.Status, &analyze, &candidates, &plan, &validation, &applyResult, &diff, &model, &prompt, &runError, &created)
	if err != nil {
		return err
	}
	run.Analyze, run.Candidates, run.Plan = rawOrNil(analyze), rawOrNil(candidates), rawOrNil(plan)
	run.Validation, run.ApplyResult, run.Diff = rawOrNil(validation), rawOrNil(applyResult), rawOrNil(diff)
	run.Model, run.PromptVersion, run.Error = model.String, prompt.String, runError.String
	run.CreatedAt, err = time.Parse(time.RFC3339Nano, created.String)
	return err
}

func rawOrNil(value sql.NullString) json.RawMessage {
	if !value.Valid || value.String == "" {
		return nil
	}
	return json.RawMessage(value.String)
}

func (s Service) ValidatePlan(ctx context.Context, plan domain.CompilationPlan, documentID string) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	if plan.DocumentID != documentID {
		return errors.New("plan document_id mismatch")
	}
	for i, action := range plan.PageActions {
		var count int
		if action.Action == domain.ActionCreate {
			if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM wiki_pages WHERE slug = ?`, action.Slug).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("page action %d creates existing slug %q", i, action.Slug)
			}
		} else if action.Action != domain.ActionNoOp {
			if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM wiki_pages WHERE id = ?`, action.TargetPageID).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				return fmt.Errorf("page action %d targets missing page %q", i, action.TargetPageID)
			}
		}
		if action.Action == domain.ActionLink || action.Action == domain.ActionMerge {
			if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM wiki_pages WHERE id = ?`, action.SourcePageID).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				return fmt.Errorf("page action %d references missing source page %q", i, action.SourcePageID)
			}
		}
		for j, claim := range action.ClaimActions {
			for _, chunkID := range claim.EvidenceChunkIDs {
				if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM source_chunks WHERE id = ? AND document_id = ?`, chunkID, documentID).Scan(&count); err != nil {
					return err
				}
				if count == 0 {
					return fmt.Errorf("claim action %d.%d references missing chunk %q", i, j, chunkID)
				}
			}
			if claim.Action == "REVISE" || claim.Action == "SUPERSEDE" || claim.Action == "MARK_DISPUTED" {
				if claim.TargetClaimID == "" && claim.PreviousText == "" {
					return fmt.Errorf("claim action %d.%d requires a target claim", i, j)
				}
				if claim.TargetClaimID != "" {
					if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM wiki_claims WHERE id = ? AND page_id = ?`, claim.TargetClaimID, action.TargetPageID).Scan(&count); err != nil {
						return err
					}
				} else if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM wiki_claims WHERE page_id = ? AND text = ?`, action.TargetPageID, claim.PreviousText).Scan(&count); err != nil {
					return err
				}
				if count == 0 {
					return fmt.Errorf("claim action %d.%d targets missing claim", i, j)
				}
			}
		}
	}
	return nil
}

type Diff struct {
	Pages          []string `json:"pages"`
	ClaimsAdded    int      `json:"claims_added"`
	ClaimsRevised  int      `json:"claims_revised"`
	ClaimsDisputed int      `json:"claims_disputed"`
	LinksAdded     int      `json:"links_added"`
	Revisions      int      `json:"revisions"`
}

func (s Service) ApplyPlan(ctx context.Context, runID string, plan domain.CompilationPlan) (Diff, error) {
	if err := s.ValidatePlan(ctx, plan, plan.DocumentID); err != nil {
		return Diff{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Diff{}, err
	}
	defer tx.Rollback()
	diff := Diff{}
	processed := 0
	for _, action := range plan.PageActions {
		if action.Action == domain.ActionNoOp {
			continue
		}
		if s.FailAfter > 0 && processed >= s.FailAfter {
			return Diff{}, fmt.Errorf("apply failure injected at action %d", processed)
		}
		processed++
		pageID := action.TargetPageID
		page, err := loadPageTx(ctx, tx, pageID)
		if action.Action == domain.ActionCreate {
			pageID = newID("page", action.Slug)
			now := time.Now().UTC()
			page = domain.WikiPage{ID: pageID, Slug: action.Slug, PageType: action.PageType, Title: action.Title, Summary: action.Summary, Status: domain.PageStatusActive, CreatedAt: now, UpdatedAt: now}
			_, err = tx.ExecContext(ctx, `INSERT INTO wiki_pages (id, slug, page_type, title, summary, status, current_revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)`, page.ID, page.Slug, page.PageType, page.Title, page.Summary, page.Status, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
		}
		if err != nil {
			return Diff{}, err
		}
		changed := action.Action == domain.ActionCreate
		for _, claim := range action.ClaimActions {
			if claim.Action == "RETAIN" {
				continue
			}
			targetID, targetErr := targetClaimID(ctx, tx, pageID, claim)
			if targetErr != nil && (claim.Action == "REVISE" || claim.Action == "SUPERSEDE" || claim.Action == "MARK_DISPUTED") {
				return Diff{}, targetErr
			}
			switch claim.Action {
			case "ADD":
				var claimID string
				err = tx.QueryRowContext(ctx, `SELECT id FROM wiki_claims WHERE page_id = ? AND text = ? AND status != 'superseded' ORDER BY id LIMIT 1`, pageID, claim.Text).Scan(&claimID)
				if err == sql.ErrNoRows {
					claimID = newID("claim", runID+pageID+claim.Text)
					claimType := claim.ClaimType
					if claimType == "" {
						claimType = domain.ClaimFact
					}
					_, err = tx.ExecContext(ctx, `INSERT INTO wiki_claims (id, page_id, text, claim_type, status, created_by_run_id, updated_by_run_id) VALUES (?, ?, ?, ?, 'active', ?, ?)`, claimID, pageID, claim.Text, claimType, runID, runID)
					if err == nil {
						diff.ClaimsAdded++
						changed = true
					}
				} else if err != nil {
					return Diff{}, err
				}
				if err == nil {
					for _, chunkID := range claim.EvidenceChunkIDs {
						if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO claim_evidence (claim_id, source_chunk_id, relation) VALUES (?, ?, 'supports')`, claimID, chunkID); err != nil {
							return Diff{}, err
						}
						changed = true
					}
				}
			case "REVISE", "SUPERSEDE":
				if _, err = tx.ExecContext(ctx, `UPDATE wiki_claims SET status = 'superseded', updated_by_run_id = ? WHERE id = ?`, runID, targetID); err != nil {
					return Diff{}, err
				}
				claimType := claim.ClaimType
				if claimType == "" {
					claimType = domain.ClaimFact
				}
				newClaimID := newID("claim", runID+pageID+claim.Text)
				if _, err = tx.ExecContext(ctx, `INSERT INTO wiki_claims (id, page_id, text, claim_type, status, created_by_run_id, updated_by_run_id) VALUES (?, ?, ?, ?, 'active', ?, ?)`, newClaimID, pageID, claim.Text, claimType, runID, runID); err != nil {
					return Diff{}, err
				}
				for _, chunkID := range claim.EvidenceChunkIDs {
					if _, err = tx.ExecContext(ctx, `INSERT INTO claim_evidence (claim_id, source_chunk_id, relation) VALUES (?, ?, 'supports')`, newClaimID, chunkID); err != nil {
						return Diff{}, err
					}
				}
				diff.ClaimsRevised++
				changed = true
			case "MARK_DISPUTED":
				if _, err = tx.ExecContext(ctx, `UPDATE wiki_claims SET status = 'disputed', updated_by_run_id = ? WHERE id = ?`, runID, targetID); err != nil {
					return Diff{}, err
				}
				for _, chunkID := range claim.EvidenceChunkIDs {
					if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO claim_evidence (claim_id, source_chunk_id, relation) VALUES (?, ?, 'disputes')`, targetID, chunkID); err != nil {
						return Diff{}, err
					}
				}
				diff.ClaimsDisputed++
				changed = true
			default:
				return Diff{}, fmt.Errorf("unsupported claim action %q", claim.Action)
			}
		}
		if action.Title != "" && action.Title != page.Title {
			page.Title = action.Title
			changed = true
		}
		if action.Summary != "" && action.Summary != page.Summary {
			page.Summary = action.Summary
			changed = true
		}
		if action.Action == domain.ActionLink {
			result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id) VALUES (?, ?, ?, ?)`, action.SourcePageID, pageID, action.Relation, runID)
			if err != nil {
				return Diff{}, err
			}
			if n, _ := result.RowsAffected(); n > 0 {
				diff.LinksAdded++
				changed = true
			}
		}
		if action.Action == domain.ActionMerge {
			sourcePage, sourceErr := loadPageTx(ctx, tx, action.SourcePageID)
			if sourceErr != nil {
				return Diff{}, sourceErr
			}
			if _, err = tx.ExecContext(ctx, `UPDATE wiki_claims SET page_id = ?, section_id = NULL, updated_by_run_id = ? WHERE page_id = ?`, pageID, runID, action.SourcePageID); err != nil {
				return Diff{}, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id)
				SELECT CASE WHEN source_page_id = ? THEN ? ELSE source_page_id END,
				       CASE WHEN target_page_id = ? THEN ? ELSE target_page_id END,
				       relation, ?
				FROM wiki_links
				WHERE (source_page_id = ? OR target_page_id = ?)
				  AND CASE WHEN source_page_id = ? THEN ? ELSE source_page_id END != CASE WHEN target_page_id = ? THEN ? ELSE target_page_id END`,
				action.SourcePageID, pageID, action.SourcePageID, pageID, runID,
				action.SourcePageID, action.SourcePageID,
				action.SourcePageID, pageID, action.SourcePageID, pageID); err != nil {
				return Diff{}, err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM wiki_links WHERE source_page_id = ? OR target_page_id = ?`, action.SourcePageID, action.SourcePageID); err != nil {
				return Diff{}, err
			}
			sourcePage.Status = domain.PageStatusMerged
			sourcePage.CurrentRevision++
			sourcePage.UpdatedAt = time.Now().UTC()
			if _, err = tx.ExecContext(ctx, `UPDATE wiki_pages SET status = ?, current_revision = ?, updated_at = ? WHERE id = ?`, sourcePage.Status, sourcePage.CurrentRevision, sourcePage.UpdatedAt.Format(time.RFC3339Nano), action.SourcePageID); err != nil {
				return Diff{}, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id) VALUES (?, ?, 'merged_into', ?)`, action.SourcePageID, pageID, runID); err != nil {
				return Diff{}, err
			}
			diff.LinksAdded++
			snapshot, snapshotErr := snapshotTx(ctx, tx, sourcePage)
			if snapshotErr != nil {
				return Diff{}, snapshotErr
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO wiki_revisions (page_id, revision_number, compilation_run_id, snapshot_json, change_summary, created_at) VALUES (?, ?, ?, ?, ?, ?)`, sourcePage.ID, sourcePage.CurrentRevision, runID, string(snapshot), action.Reason, sourcePage.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
				return Diff{}, err
			}
			diff.Revisions++
			diff.Pages = append(diff.Pages, sourcePage.ID)
			changed = true
		}
		if changed {
			page.CurrentRevision++
			page.UpdatedAt = time.Now().UTC()
			if _, err = tx.ExecContext(ctx, `UPDATE wiki_pages SET title = ?, summary = ?, current_revision = ?, updated_at = ? WHERE id = ?`, page.Title, page.Summary, page.CurrentRevision, page.UpdatedAt.Format(time.RFC3339Nano), page.ID); err != nil {
				return Diff{}, err
			}
			snapshot, err := snapshotTx(ctx, tx, page)
			if err != nil {
				return Diff{}, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO wiki_revisions (page_id, revision_number, compilation_run_id, snapshot_json, change_summary, created_at) VALUES (?, ?, ?, ?, ?, ?)`, page.ID, page.CurrentRevision, runID, string(snapshot), action.Reason, page.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
				return Diff{}, err
			}
			diff.Revisions++
			diff.Pages = append(diff.Pages, page.ID)
		}
	}
	sort.Strings(diff.Pages)
	diffRaw, err := json.Marshal(diff)
	if err != nil {
		return Diff{}, err
	}
	// The Wiki transaction is committed only after the run becomes
	// render_pending. A crash between Apply and projection therefore leaves a
	// retryable run rather than an apparently failed or still-validated run.
	result, err := tx.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, apply_result_json = ?, diff_json = ?, error = NULL WHERE id = ?`, RunRenderPending, `{"applied":true,"rendered":false}`, string(diffRaw), runID)
	if err != nil {
		return diff, err
	}
	if updated, _ := result.RowsAffected(); updated != 1 {
		return diff, fmt.Errorf("compilation run %s does not exist", runID)
	}
	if err = tx.Commit(); err != nil {
		return Diff{}, err
	}
	render := s.Render
	if render == nil {
		if err := s.renderDefaultProjection(ctx); err != nil {
			return diff, err
		}
	} else {
		pages, claims, evidence, err := s.loadWikiState(ctx)
		if err != nil {
			return diff, &RenderPendingError{Cause: err}
		}
		if err = render(s.DataRoot, pages, claims, evidence); err != nil {
			return diff, &RenderPendingError{Cause: err}
		}
	}
	return diff, nil
}

func loadPageTx(ctx context.Context, tx *sql.Tx, id string) (domain.WikiPage, error) {
	var page domain.WikiPage
	var created, updated string
	err := tx.QueryRowContext(ctx, `SELECT id, slug, page_type, title, summary, status, current_revision, created_at, updated_at FROM wiki_pages WHERE id = ?`, id).Scan(&page.ID, &page.Slug, &page.PageType, &page.Title, &page.Summary, &page.Status, &page.CurrentRevision, &created, &updated)
	if err == nil {
		page.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		page.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	}
	return page, err
}

func targetClaimID(ctx context.Context, tx *sql.Tx, pageID string, claim domain.ClaimAction) (string, error) {
	if claim.TargetClaimID != "" {
		return claim.TargetClaimID, nil
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM wiki_claims WHERE page_id = ? AND text = ? ORDER BY id LIMIT 1`, pageID, claim.PreviousText).Scan(&id)
	return id, err
}

func snapshotTx(ctx context.Context, tx *sql.Tx, page domain.WikiPage) ([]byte, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, page_id, COALESCE(section_id, ''), text, claim_type, status, created_by_run_id, updated_by_run_id FROM wiki_claims WHERE page_id = ? ORDER BY id`, page.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	claims := make([]domain.WikiClaim, 0)
	for rows.Next() {
		var claim domain.WikiClaim
		if err := rows.Scan(&claim.ID, &claim.PageID, &claim.SectionID, &claim.Text, &claim.ClaimType, &claim.Status, &claim.CreatedByRunID, &claim.UpdatedByRunID); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Page   domain.WikiPage    `json:"page"`
		Claims []domain.WikiClaim `json:"claims"`
	}{page, claims})
}

func (s Service) match(ctx context.Context, analysis domain.SourceAnalysis) ([]domain.CompilationCandidate, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, slug, title, page_type, summary FROM wiki_pages WHERE status = 'active' ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pages := make([]domain.CompilationCandidate, 0)
	for rows.Next() {
		var c domain.CompilationCandidate
		if err := rows.Scan(&c.PageID, &c.Slug, &c.Title, &c.PageType, &c.Summary); err != nil {
			return nil, err
		}
		pages = append(pages, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range pages {
		claimRows, err := s.DB.QueryContext(ctx, `SELECT text FROM wiki_claims WHERE page_id = ? AND status = 'active' ORDER BY id`, pages[i].PageID)
		if err != nil {
			return nil, err
		}
		for claimRows.Next() {
			var text string
			if err := claimRows.Scan(&text); err != nil {
				claimRows.Close()
				return nil, err
			}
			pages[i].ClaimTexts = append(pages[i].ClaimTexts, text)
		}
		claimRows.Close()
	}
	const maxCandidatesPerTopic = 5
	result := make([]domain.CompilationCandidate, 0)
	for _, topic := range analysis.Topics {
		topicCandidates := make([]domain.CompilationCandidate, 0, len(pages))
		for _, page := range pages {
			page.TopicKey = topic.Key
			page.Score = lexicalSimilarity(topic.Title+" "+topic.Slug, page.Title+" "+page.Slug)
			if topic.Slug == page.Slug || strings.EqualFold(topic.Title, page.Title) {
				page.Score = 1
			}
			if page.Score >= 0.20 {
				topicCandidates = append(topicCandidates, page)
			}
		}
		sort.Slice(topicCandidates, func(i, j int) bool {
			if topicCandidates[i].Score != topicCandidates[j].Score {
				return topicCandidates[i].Score > topicCandidates[j].Score
			}
			return topicCandidates[i].Slug < topicCandidates[j].Slug
		})
		if len(topicCandidates) > maxCandidatesPerTopic {
			topicCandidates = topicCandidates[:maxCandidatesPerTopic]
		}
		result = append(result, topicCandidates...)
	}
	return result, nil
}

func lexicalSimilarity(left, right string) float64 {
	a := normalizedTokens(left)
	b := normalizedTokens(right)
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for token := range a {
		if b[token] {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	jaccard := float64(intersection) / float64(union)
	containment := float64(intersection) / float64(min(len(a), len(b)))
	return 0.6*jaccard + 0.4*containment
}

func normalizedTokens(value string) map[string]bool {
	result := make(map[string]bool)
	var latin strings.Builder
	flushLatin := func() {
		if latin.Len() == 0 {
			return
		}
		token := latin.String()
		if len(token) > 3 && strings.HasSuffix(token, "s") {
			token = strings.TrimSuffix(token, "s")
		}
		switch token {
		case "the", "a", "an", "of", "and", "for", "system":
		default:
			result[token] = true
		}
		latin.Reset()
	}
	for _, r := range strings.ToLower(value) {
		if unicode.Is(unicode.Han, r) {
			flushLatin()
			// Bigrams retain useful overlap for Chinese titles without adding a
			// language-specific dictionary or heavyweight tokenizer.
			continue
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			latin.WriteRune(r)
			continue
		}
		flushLatin()
	}
	flushLatin()

	var han []rune
	flushHan := func() {
		if len(han) == 0 {
			return
		}
		if len(han) == 1 {
			result[string(han[0])] = true
		} else {
			for i := 0; i < len(han)-1; i++ {
				result[string(han[i:i+2])] = true
			}
		}
		han = nil
	}
	for _, r := range strings.ToLower(value) {
		if unicode.Is(unicode.Han, r) {
			han = append(han, r)
			continue
		}
		flushHan()
	}
	flushHan()
	return result
}

func validateAnalysis(analysis domain.SourceAnalysis, chunks []domain.SourceChunk) error {
	if analysis.DocumentID == "" || strings.TrimSpace(analysis.Summary) == "" || len(analysis.Topics) == 0 {
		return errors.New("analysis requires document_id, summary, and topics")
	}
	valid := map[string]bool{}
	for _, chunk := range chunks {
		valid[chunk.ID] = true
	}
	topicKeys := map[string]bool{}
	topicSlugs := map[string]bool{}
	for _, topic := range analysis.Topics {
		if topic.Title == "" || topic.Slug == "" || topic.Summary == "" || !validPageType(topic.PageType) {
			return errors.New("analysis contains invalid topic")
		}
		if topic.Key == "" || topicKeys[topic.Key] || topicSlugs[topic.Slug] {
			return errors.New("analysis contains duplicate or missing topic key/slug")
		}
		topicKeys[topic.Key] = true
		topicSlugs[topic.Slug] = true
		for _, claim := range topic.Claims {
			if claim.Text == "" || !validClaimType(claim.ClaimType) || len(claim.EvidenceChunkIDs) == 0 {
				return errors.New("analysis contains invalid claim")
			}
			for _, id := range claim.EvidenceChunkIDs {
				if !valid[id] {
					return fmt.Errorf("analysis references missing chunk %q", id)
				}
			}
		}
	}
	for _, relation := range analysis.Relations {
		if relation.SourceTopicKey == "" || relation.TargetTopicKey == "" || relation.SourceTopicKey == relation.TargetTopicKey || !topicKeys[relation.SourceTopicKey] || !topicKeys[relation.TargetTopicKey] || !validRelation(relation.Relation) {
			return errors.New("analysis contains invalid relation")
		}
	}
	return nil
}

func (s Service) renderDefaultProjection(ctx context.Context) error {
	pages, claims, evidence, err := s.loadWikiState(ctx)
	if err != nil {
		return &RenderPendingError{Cause: err}
	}
	links, err := s.loadWikiLinks(ctx)
	if err != nil {
		return &RenderPendingError{Cause: err}
	}
	if err := RenderMarkdownWithLinks(s.DataRoot, pages, claims, evidence, links); err != nil {
		return &RenderPendingError{Cause: err}
	}
	return nil
}

func validRelation(value string) bool {
	switch value {
	case "related_to", "part_of", "depends_on", "contradicts", "supports", "references":
		return true
	default:
		return false
	}
}

func validPageType(value domain.PageType) bool {
	return value == domain.PageTypeConcept || value == domain.PageTypeEntity || value == domain.PageTypeTopic
}
func validClaimType(value domain.ClaimType) bool {
	return value == domain.ClaimFact || value == domain.ClaimDefinition || value == domain.ClaimArgument || value == domain.ClaimProcedure || value == domain.ClaimCaveat
}
func newID(prefix, value string) string {
	sum := sha256.Sum256([]byte(value))
	return prefix + "_" + hex.EncodeToString(sum[:])
}
func nowString() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s Service) loadDocument(ctx context.Context, id string) (domain.SourceDocument, error) {
	var d domain.SourceDocument
	var created string
	err := s.DB.QueryRowContext(ctx, `SELECT id, original_name, media_type, sha256, original_path, COALESCE(parsed_path, ''), status, parser_version, created_at, COALESCE(parse_error, '') FROM source_documents WHERE id = ?`, id).Scan(&d.ID, &d.OriginalName, &d.MediaType, &d.SHA256, &d.OriginalPath, &d.ParsedPath, &d.Status, &d.ParserVersion, &created, &d.ParseError)
	if err != nil {
		return d, err
	}
	d.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return d, err
}

func (s Service) loadChunks(ctx context.Context, id string) ([]domain.SourceChunk, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, document_id, chunk_index, text, page_number, heading_path, char_start, char_end, content_hash FROM source_chunks WHERE document_id = ? ORDER BY chunk_index`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.SourceChunk, 0)
	for rows.Next() {
		var c domain.SourceChunk
		var page sql.NullInt64
		var heading string
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.ChunkIndex, &c.Text, &page, &heading, &c.CharStart, &c.CharEnd, &c.ContentHash); err != nil {
			return nil, err
		}
		if page.Valid {
			p := int(page.Int64)
			c.PageNumber = &p
		}
		_ = json.Unmarshal([]byte(heading), &c.HeadingPath)
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s Service) loadWikiState(ctx context.Context) ([]domain.WikiPage, []domain.WikiClaim, []domain.ClaimEvidence, error) {
	pageRows, err := s.DB.QueryContext(ctx, `SELECT id, slug, page_type, title, summary, status, current_revision, created_at, updated_at FROM wiki_pages WHERE status = 'active' ORDER BY slug`)
	if err != nil {
		return nil, nil, nil, err
	}
	defer pageRows.Close()
	pages := make([]domain.WikiPage, 0)
	for pageRows.Next() {
		var page domain.WikiPage
		var created, updated string
		if err := pageRows.Scan(&page.ID, &page.Slug, &page.PageType, &page.Title, &page.Summary, &page.Status, &page.CurrentRevision, &created, &updated); err != nil {
			return nil, nil, nil, err
		}
		page.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		page.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		pages = append(pages, page)
	}
	if err := pageRows.Err(); err != nil {
		return nil, nil, nil, err
	}
	claimRows, err := s.DB.QueryContext(ctx, `SELECT c.id, c.page_id, COALESCE(c.section_id, ''), c.text, c.claim_type, c.status, c.created_by_run_id, c.updated_by_run_id FROM wiki_claims c JOIN wiki_pages p ON p.id = c.page_id WHERE p.status = 'active' ORDER BY c.page_id, c.id`)
	if err != nil {
		return nil, nil, nil, err
	}
	defer claimRows.Close()
	claims := make([]domain.WikiClaim, 0)
	for claimRows.Next() {
		var claim domain.WikiClaim
		if err := claimRows.Scan(&claim.ID, &claim.PageID, &claim.SectionID, &claim.Text, &claim.ClaimType, &claim.Status, &claim.CreatedByRunID, &claim.UpdatedByRunID); err != nil {
			return nil, nil, nil, err
		}
		claims = append(claims, claim)
	}
	evidenceRows, err := s.DB.QueryContext(ctx, `SELECT ce.claim_id, ce.source_chunk_id, ce.relation, COALESCE(ce.note, '') FROM claim_evidence ce JOIN wiki_claims c ON c.id = ce.claim_id JOIN wiki_pages p ON p.id = c.page_id WHERE p.status = 'active' ORDER BY ce.claim_id, ce.source_chunk_id`)
	if err != nil {
		return nil, nil, nil, err
	}
	defer evidenceRows.Close()
	evidence := make([]domain.ClaimEvidence, 0)
	for evidenceRows.Next() {
		var item domain.ClaimEvidence
		if err := evidenceRows.Scan(&item.ClaimID, &item.SourceChunkID, &item.Relation, &item.Note); err != nil {
			return nil, nil, nil, err
		}
		evidence = append(evidence, item)
	}
	return pages, claims, evidence, evidenceRows.Err()
}

func (s Service) loadWikiLinks(ctx context.Context) ([]domain.WikiLink, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT l.source_page_id, l.target_page_id, l.relation, l.created_by_run_id FROM wiki_links l JOIN wiki_pages sp ON sp.id = l.source_page_id AND sp.status = 'active' JOIN wiki_pages tp ON tp.id = l.target_page_id AND tp.status = 'active' ORDER BY l.source_page_id, l.target_page_id, l.relation`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := make([]domain.WikiLink, 0)
	for rows.Next() {
		var link domain.WikiLink
		if err := rows.Scan(&link.SourcePageID, &link.TargetPageID, &link.Relation, &link.CreatedByRunID); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

func RenderMarkdown(dataRoot string, pages []domain.WikiPage, claims []domain.WikiClaim, evidence []domain.ClaimEvidence) error {
	return RenderMarkdownWithLinks(dataRoot, pages, claims, evidence, nil)
}

func RenderMarkdownWithLinks(dataRoot string, pages []domain.WikiPage, claims []domain.WikiClaim, evidence []domain.ClaimEvidence, links []domain.WikiLink) error {
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(dataRoot, ".wiki-render-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := renderMarkdownFiles(staging, pages, claims, evidence, links); err != nil {
		return err
	}

	target := filepath.Join(dataRoot, "wiki")
	backup := filepath.Join(dataRoot, fmt.Sprintf(".wiki-backup-%d", time.Now().UnixNano()))
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			return err
		}
		if err := os.Rename(staging, target); err != nil {
			_ = os.Rename(backup, target)
			return err
		}
		return os.RemoveAll(backup)
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(staging, target)
}

func renderMarkdownFiles(outputDir string, pages []domain.WikiPage, claims []domain.WikiClaim, evidence []domain.ClaimEvidence, links []domain.WikiLink) error {
	activePages := pages[:0]
	for _, page := range pages {
		if page.Status == domain.PageStatusActive {
			activePages = append(activePages, page)
		}
	}
	pages = activePages
	claimByPage := map[string][]domain.WikiClaim{}
	evidenceByClaim := map[string][]domain.ClaimEvidence{}
	pageByID := map[string]domain.WikiPage{}
	linksBySource := map[string][]domain.WikiLink{}
	for _, page := range pages {
		pageByID[page.ID] = page
	}
	for _, link := range links {
		if _, sourceActive := pageByID[link.SourcePageID]; !sourceActive {
			continue
		}
		if _, targetActive := pageByID[link.TargetPageID]; !targetActive {
			continue
		}
		linksBySource[link.SourcePageID] = append(linksBySource[link.SourcePageID], link)
	}
	for _, claim := range claims {
		claimByPage[claim.PageID] = append(claimByPage[claim.PageID], claim)
	}
	for _, item := range evidence {
		evidenceByClaim[item.ClaimID] = append(evidenceByClaim[item.ClaimID], item)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].Slug < pages[j].Slug })
	for _, pageType := range []domain.PageType{domain.PageTypeConcept, domain.PageTypeEntity, domain.PageTypeTopic} {
		if err := os.MkdirAll(filepath.Join(outputDir, string(pageType)+"s"), 0o755); err != nil {
			return err
		}
	}
	var index strings.Builder
	index.WriteString("# Compiled Wiki\n\n")
	index.WriteString("Deterministic index of compiled knowledge pages.\n\n")
	for _, page := range pages {
		if !validRenderSlug(page.Slug) {
			return fmt.Errorf("refuse to render wiki page with unsafe slug %q", page.Slug)
		}
		fmt.Fprintf(&index, "- [%s](%ss/%s.md)\n", page.Title, page.PageType, page.Slug)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "index.md"), []byte(index.String()), 0o644); err != nil {
		return err
	}
	for _, page := range pages {
		if !validRenderSlug(page.Slug) {
			return fmt.Errorf("refuse to render wiki page with unsafe slug %q", page.Slug)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "---\nslug: %s\npage_type: %s\nstatus: %s\nrevision: %d\n---\n\n# %s\n\n%s\n\n## Claims\n", page.Slug, page.PageType, page.Status, page.CurrentRevision, page.Title, page.Summary)
		items := claimByPage[page.ID]
		sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
		for _, claim := range items {
			if claim.Status != "active" && claim.Status != "disputed" {
				continue
			}
			fmt.Fprintf(&b, "\n- **%s**: %s", claim.ClaimType, claim.Text)
			citations := evidenceByClaim[claim.ID]
			sort.Slice(citations, func(i, j int) bool { return citations[i].SourceChunkID < citations[j].SourceChunkID })
			for _, citation := range citations {
				fmt.Fprintf(&b, " [%s:%s]", citation.Relation, citation.SourceChunkID)
			}
		}
		if related := linksBySource[page.ID]; len(related) > 0 {
			sort.Slice(related, func(i, j int) bool {
				left, right := pageByID[related[i].TargetPageID], pageByID[related[j].TargetPageID]
				if left.Slug != right.Slug {
					return left.Slug < right.Slug
				}
				return related[i].Relation < related[j].Relation
			})
			b.WriteString("\n\n## Related Pages\n")
			for _, link := range related {
				target := pageByID[link.TargetPageID]
				fmt.Fprintf(&b, "\n- [[%s]] (`%s`)", target.Slug, link.Relation)
			}
		}
		b.WriteString("\n")
		content := []byte(b.String())
		for _, path := range []string{
			filepath.Join(outputDir, page.Slug+".md"),
			filepath.Join(outputDir, string(page.PageType)+"s", page.Slug+".md"),
		} {
			if err := os.WriteFile(path, content, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func validRenderSlug(slug string) bool {
	if slug == "" || strings.Contains(slug, "..") || strings.Contains(slug, "--") || strings.ContainsAny(slug, `/\\`) {
		return false
	}
	for i, r := range slug {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '-' && i > 0 && i < len(slug)-1) {
			continue
		}
		return false
	}
	return true
}
