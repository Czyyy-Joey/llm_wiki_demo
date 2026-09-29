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
	"sync"
	"time"
	"unicode"

	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/indexing"
	"github.com/joeychen/llm-wiki-demo/backend/internal/knowledgebase"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	"github.com/joeychen/llm-wiki-demo/backend/internal/logging"
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
	DB              *sql.DB
	DataRoot        string
	LLM             llm.LLMClient
	Index           *indexing.Service
	Render          func(string, []domain.WikiPage, []domain.WikiClaim, []domain.ClaimEvidence) error
	FailAfter       int // 0 disables injection; a positive value fails before that action index.
	KnowledgeBaseID string
	Language        string
}

func (s Service) scope(ctx context.Context) string {
	return knowledgebase.Scope(ctx, s.KnowledgeBaseID)
}

// compileGates serializes compilation within a single knowledge base. The
// analyze→match→plan→validate→apply pipeline spans many separate statements and
// LLM calls, so overlapping runs for the same knowledge base race on checks like
// slug uniqueness. The Service struct is rebuilt per request, so the registry
// must live at package scope to be shared across requests; different knowledge
// bases keep independent gates and still run in parallel.
var (
	compileGatesMu sync.Mutex
	compileGates   = map[string]chan struct{}{}
)

func compileGate(knowledgeBaseID string) chan struct{} {
	compileGatesMu.Lock()
	defer compileGatesMu.Unlock()
	gate, ok := compileGates[knowledgeBaseID]
	if !ok {
		gate = make(chan struct{}, 1)
		compileGates[knowledgeBaseID] = gate
	}
	return gate
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
	ctx = llm.WithLanguage(ctx, s.Language)
	scope := s.scope(ctx)
	started := time.Now()
	stage := "load_source"
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
	client := s.LLM
	if client == nil {
		return Result{}, errors.New("compiler LLM is not configured; configure COMPILER_LLM_* or explicitly enable the development fake")
	}
	// Content dedup + coalesce keyed on the source's latest run. The document ID is
	// derived from the source content hash, so an applied or render-pending run means
	// this exact content is already compiled, and any non-terminal run means it is
	// already queued or running — either way return that run instead of compiling
	// again. Only a failed last run is recompiled.
	var lastID, lastStatus string
	switch scanErr := s.DB.QueryRowContext(ctx, `SELECT id, status FROM compilation_runs WHERE document_id = ? AND knowledge_base_id = ? ORDER BY created_at DESC LIMIT 1`, documentID, scope).Scan(&lastID, &lastStatus); scanErr {
	case nil:
		if lastStatus != RunFailed {
			return Result{RunID: lastID, Status: lastStatus}, nil
		}
	case sql.ErrNoRows:
	default:
		return Result{}, scanErr
	}
	runID := newID("run", scope+documentID+time.Now().UTC().Format(time.RFC3339Nano))
	model := "configured-client"
	if named, ok := client.(interface{ Name() string }); ok {
		model = named.Name()
	}
	// Record the run as pending before waiting on the gate so a queued compilation
	// is visible immediately (e.g. on the compilation page) while an earlier run for
	// the same knowledge base is still in progress, instead of only once it starts.
	if _, err = s.DB.ExecContext(ctx, `INSERT INTO compilation_runs (id, knowledge_base_id, document_id, status, model, prompt_version, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, runID, scope, documentID, RunPending, model, "phase2-v2", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return Result{}, err
	}
	logging.Logger(ctx).Info("compilation queued", "run_id", runID, "document_id", documentID)
	// Serialize compilation per knowledge base. If the client disconnects while
	// queued, mark the pending run failed so it does not linger as a stuck row.
	gate := compileGate(scope)
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		_, _ = s.DB.ExecContext(context.Background(), `UPDATE compilation_runs SET status = ?, error = ? WHERE id = ? AND knowledge_base_id = ?`, RunFailed, ctx.Err().Error(), runID, scope)
		return Result{RunID: runID, Status: RunFailed}, ctx.Err()
	}
	logging.Logger(ctx).Info("compilation started", "run_id", runID, "document_id", documentID)
	fail := func(cause error) (Result, error) {
		_, _ = s.DB.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, error = ? WHERE id = ? AND knowledge_base_id = ?`, RunFailed, cause.Error(), runID, scope)
		logging.Logger(ctx).Error("compilation failed", "run_id", runID, "document_id", documentID, "stage", stage, "status", RunFailed, "duration_ms", logging.Duration(started), "error", logging.SafeDetail(cause))
		return Result{RunID: runID, Status: RunFailed}, cause
	}
	stage = "analyze"
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
	logging.Logger(ctx).Info("compilation analyze completed", "run_id", runID, "topics", len(analysis.Topics), "duration_ms", logging.Duration(started))
	if _, err = s.DB.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, analyze_json = ? WHERE id = ? AND knowledge_base_id = ?`, RunAnalyzed, string(analyzeRaw), runID, s.scope(ctx)); err != nil {
		return fail(err)
	}
	stage = "match"
	candidates, err := s.match(ctx, analysis)
	if err != nil {
		return fail(err)
	}
	candidatesRaw, _ := json.Marshal(candidates)
	if _, err = s.DB.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, candidates_json = ? WHERE id = ? AND knowledge_base_id = ?`, RunPlanned, string(candidatesRaw), runID, s.scope(ctx)); err != nil {
		return fail(err)
	}
	logging.Logger(ctx).Info("compilation match completed", "run_id", runID, "candidates", len(candidates), "duration_ms", logging.Duration(started))
	stage = "plan"
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
	logging.Logger(ctx).Info("compilation plan completed", "run_id", runID, "actions", len(plan.PageActions), "duration_ms", logging.Duration(started))
	stage = "validate"
	if err = s.ValidatePlan(ctx, plan, documentID); err != nil {
		logging.Logger(ctx).Error("compilation failed", "run_id", runID, "document_id", documentID, "stage", stage, "status", RunFailed, "duration_ms", logging.Duration(started), "error", logging.SafeDetail(err))
		return failWithPlan(ctx, s.DB, runID, s.scope(ctx), planRaw, err)
	}
	logging.Logger(ctx).Info("compilation validation completed", "run_id", runID, "actions", len(plan.PageActions), "duration_ms", logging.Duration(started))
	if _, err = s.DB.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, plan_json = ?, validation_json = ? WHERE id = ? AND knowledge_base_id = ?`, RunValidated, string(planRaw), `{"valid":true}`, runID, s.scope(ctx)); err != nil {
		return fail(err)
	}
	stage = "apply"
	diff, err := s.ApplyPlan(ctx, runID, plan)
	if err != nil {
		var renderErr *RenderPendingError
		if errors.As(err, &renderErr) {
			logging.Logger(ctx).Error("compilation render pending", "run_id", runID, "document_id", documentID, "stage", "render", "status", RunRenderPending, "duration_ms", logging.Duration(started), "error", logging.SafeSummary(err))
			return Result{RunID: runID, Status: RunRenderPending}, nil
		}
		logging.Logger(ctx).Error("compilation failed", "run_id", runID, "document_id", documentID, "stage", stage, "status", RunFailed, "duration_ms", logging.Duration(started), "error", logging.SafeSummary(err))
		return failWithPlan(ctx, s.DB, runID, s.scope(ctx), planRaw, err)
	}
	diffRaw, _ := json.Marshal(diff)
	if s.Index != nil {
		stage = "index"
		logging.Logger(ctx).Info("compilation indexing started", "run_id", runID)
		if _, indexErr := s.Index.Reindex(ctx); indexErr != nil {
			logging.Logger(ctx).Error("compilation indexing failed", "run_id", runID, "error", logging.SafeSummary(indexErr))
			_, _ = s.DB.ExecContext(ctx, `UPDATE wiki_pages SET index_status = 'index_pending' WHERE status = 'active' AND knowledge_base_id = ?`, s.scope(ctx))
		} else {
			_, _ = s.DB.ExecContext(ctx, `UPDATE wiki_pages SET index_status = 'clean' WHERE status = 'active' AND knowledge_base_id = ?`, s.scope(ctx))
		}
	}
	if _, err = s.DB.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, apply_result_json = ?, diff_json = ? WHERE id = ? AND knowledge_base_id = ?`, RunApplied, `{"applied":true}`, string(diffRaw), runID, s.scope(ctx)); err != nil {
		// ApplyPlan has already committed the Wiki transaction and marked the run
		// render_pending. Keep that recoverable state instead of reporting a
		// failed run after the Wiki has changed.
		return Result{RunID: runID, Status: RunRenderPending}, err
	}
	logging.Logger(ctx).Info("compilation applied", "run_id", runID, "pages", len(diff.Pages), "claims_added", diff.ClaimsAdded, "links_added", diff.LinksAdded, "duration_ms", logging.Duration(started))
	stage = "relate"
	if related, relateErr := s.RelateAll(ctx, runID); relateErr != nil {
		logging.Logger(ctx).Error("cross-page relate failed", "run_id", runID, "error", logging.SafeSummary(relateErr))
	} else if related > 0 {
		logging.Logger(ctx).Info("cross-page relate completed", "run_id", runID, "links_added", related)
	}
	// Deterministic backstop: link plain title mentions the relate step missed so
	// they exist as data and are reachable through retrieval link expansion.
	if mentioned, mentionErr := s.linkMentions(ctx, runID); mentionErr != nil {
		logging.Logger(ctx).Error("mention linking failed", "run_id", runID, "error", logging.SafeSummary(mentionErr))
	} else if mentioned > 0 {
		logging.Logger(ctx).Info("mention linking completed", "run_id", runID, "links_added", mentioned)
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
	_, err = s.DB.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, apply_result_json = ?, error = NULL WHERE id = ? AND knowledge_base_id = ?`, RunApplied, `{"applied":true,"rendered":true}`, runID, s.scope(ctx))
	return err
}

func failWithPlan(ctx context.Context, db *sql.DB, runID, knowledgeBaseID string, plan json.RawMessage, cause error) (Result, error) {
	validation, _ := json.Marshal(map[string]any{"valid": false, "error": cause.Error()})
	_, _ = db.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, plan_json = ?, validation_json = ?, error = ? WHERE id = ? AND knowledge_base_id = ?`, RunFailed, string(plan), string(validation), cause.Error(), runID, knowledgeBaseID)
	return Result{RunID: runID, Status: RunFailed}, cause
}

func (s Service) GetRun(ctx context.Context, id string) (Run, error) {
	var run Run
	err := scanRun(s.DB.QueryRowContext(ctx, `SELECT id, document_id, status, analyze_json, candidates_json, plan_json, validation_json, apply_result_json, diff_json, model, prompt_version, error, created_at FROM compilation_runs WHERE id = ? AND knowledge_base_id = ?`, id, s.scope(ctx)), &run)
	return run, err
}

func (s Service) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, document_id, status, analyze_json, candidates_json, plan_json, validation_json, apply_result_json, diff_json, model, prompt_version, error, created_at FROM compilation_runs WHERE knowledge_base_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`, s.scope(ctx), limit)
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

// planCreatedSlugs collects the slugs of pages a plan creates. LINK and MERGE
// actions may reference these slugs to relate pages born in the same run,
// before those pages have database IDs.
func planCreatedSlugs(plan domain.CompilationPlan) map[string]bool {
	slugs := make(map[string]bool)
	for _, action := range plan.PageActions {
		if action.Action == domain.ActionCreate && action.Slug != "" {
			slugs[action.Slug] = true
		}
	}
	return slugs
}

// resolvePageRef maps a plan page reference to a stored page ID. A reference
// matching a slug created in the same plan resolves to that page's
// deterministic ID (identical to what ActionCreate assigns); any other
// reference is treated as an existing page ID and returned unchanged.
func (s Service) resolvePageRef(ctx context.Context, ref string, created map[string]bool) string {
	if ref != "" && created[ref] {
		return newID("page", s.scope(ctx)+":"+ref)
	}
	return ref
}

// createsFirst returns the actions with every CREATE ordered ahead of the
// rest, preserving relative order within each group. This lets a later LINK or
// MERGE reference a page the same plan creates.
func createsFirst(actions []domain.PageAction) []domain.PageAction {
	ordered := make([]domain.PageAction, 0, len(actions))
	for _, action := range actions {
		if action.Action == domain.ActionCreate {
			ordered = append(ordered, action)
		}
	}
	for _, action := range actions {
		if action.Action != domain.ActionCreate {
			ordered = append(ordered, action)
		}
	}
	return ordered
}

func (s Service) ValidatePlan(ctx context.Context, plan domain.CompilationPlan, documentID string) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	if plan.DocumentID != documentID {
		return fmt.Errorf("invalid compilation plan: field=%q value=%q constraint=%q reason=%q", "document_id", plan.DocumentID, fmt.Sprintf("must match %q", documentID), "document ID mismatch")
	}
	created := planCreatedSlugs(plan)
	for i, action := range plan.PageActions {
		pagePrefix := fmt.Sprintf("invalid page action: index=%d action=%q target_page_id=%q source_page_id=%q slug=%q title=%q", i, action.Action, action.TargetPageID, action.SourcePageID, action.Slug, diagnosticText(action.Title))
		var count int
		if action.Action == domain.ActionCreate {
			if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM wiki_pages WHERE slug = ? AND knowledge_base_id = ?`, action.Slug, s.scope(ctx)).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("%s field=%q value=%q reason=%q", pagePrefix, "slug", action.Slug, "already exists")
			}
		} else if action.Action != domain.ActionNoOp && !created[action.TargetPageID] {
			if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM wiki_pages WHERE id = ? AND knowledge_base_id = ?`, action.TargetPageID, s.scope(ctx)).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				return fmt.Errorf("%s field=%q value=%q reason=%q", pagePrefix, "target_page_id", action.TargetPageID, "page does not exist")
			}
		}
		if (action.Action == domain.ActionLink || action.Action == domain.ActionMerge) && !created[action.SourcePageID] {
			if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM wiki_pages WHERE id = ? AND knowledge_base_id = ?`, action.SourcePageID, s.scope(ctx)).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				return fmt.Errorf("%s field=%q value=%q reason=%q", pagePrefix, "source_page_id", action.SourcePageID, "source page does not exist")
			}
		}
		for j, claim := range action.ClaimActions {
			claimPrefix := fmt.Sprintf("invalid claim action: page_index=%d claim_index=%d page_action=%q target_page_id=%q claim_type=%q text=%q evidence_chunk_ids=%v", i, j, action.Action, action.TargetPageID, claim.ClaimType, diagnosticText(claim.Text), claim.EvidenceChunkIDs)
			for _, chunkID := range claim.EvidenceChunkIDs {
				if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM source_chunks WHERE id = ? AND document_id = ? AND knowledge_base_id = ?`, chunkID, documentID, s.scope(ctx)).Scan(&count); err != nil {
					return err
				}
				if count == 0 {
					return fmt.Errorf("%s field=%q value=%q reason=%q", claimPrefix, "evidence_chunk_ids", chunkID, "source chunk does not exist for this document")
				}
			}
			if claim.Action == "REVISE" || claim.Action == "SUPERSEDE" || claim.Action == "MARK_DISPUTED" {
				if claim.TargetClaimID == "" && claim.PreviousText == "" {
					return fmt.Errorf("%s field=%q reason=%q", claimPrefix, "target_claim_id/previous_text", "one is required for this claim action")
				}
				if claim.TargetClaimID != "" {
					if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM wiki_claims WHERE id = ? AND page_id = ? AND knowledge_base_id = ?`, claim.TargetClaimID, action.TargetPageID, s.scope(ctx)).Scan(&count); err != nil {
						return err
					}
				} else if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM wiki_claims WHERE page_id = ? AND text = ? AND knowledge_base_id = ?`, action.TargetPageID, claim.PreviousText, s.scope(ctx)).Scan(&count); err != nil {
					return err
				}
				if count == 0 {
					return fmt.Errorf("%s field=%q value=%q reason=%q", claimPrefix, "target_claim_id/previous_text", firstNonEmpty(claim.TargetClaimID, claim.PreviousText), "claim does not exist on the target page")
				}
			}
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
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
	created := planCreatedSlugs(plan)
	// Apply CREATE actions first so LINK and MERGE actions that reference a
	// page born in the same plan find an existing row (wiki_links enforces
	// foreign keys against wiki_pages).
	ordered := createsFirst(plan.PageActions)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Diff{}, err
	}
	defer tx.Rollback()
	diff := Diff{}
	processed := 0
	for _, action := range ordered {
		if action.Action == domain.ActionNoOp {
			continue
		}
		if s.FailAfter > 0 && processed >= s.FailAfter {
			return Diff{}, fmt.Errorf("apply failure injected at action %d", processed)
		}
		processed++
		pageID := s.resolvePageRef(ctx, action.TargetPageID, created)
		sourceID := s.resolvePageRef(ctx, action.SourcePageID, created)
		page, err := loadPageTx(ctx, tx, pageID, s.scope(ctx))
		if action.Action == domain.ActionCreate {
			pageID = newID("page", s.scope(ctx)+":"+action.Slug)
			now := time.Now().UTC()
			page = domain.WikiPage{ID: pageID, Slug: action.Slug, PageType: action.PageType, Title: action.Title, Summary: action.Summary, Status: domain.PageStatusActive, CreatedAt: now, UpdatedAt: now}
			_, err = tx.ExecContext(ctx, `INSERT INTO wiki_pages (id, knowledge_base_id, slug, page_type, title, summary, status, current_revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`, page.ID, s.scope(ctx), page.Slug, page.PageType, page.Title, page.Summary, page.Status, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
		}
		if err != nil {
			return Diff{}, err
		}
		changed := action.Action == domain.ActionCreate
		for _, claim := range action.ClaimActions {
			if claim.Action == "RETAIN" {
				continue
			}
			targetID, targetErr := targetClaimID(ctx, tx, pageID, claim, s.scope(ctx))
			if targetErr != nil && (claim.Action == "REVISE" || claim.Action == "SUPERSEDE" || claim.Action == "MARK_DISPUTED") {
				return Diff{}, targetErr
			}
			switch claim.Action {
			case "ADD":
				var claimID string
				err = tx.QueryRowContext(ctx, `SELECT id FROM wiki_claims WHERE page_id = ? AND text = ? AND status != 'superseded' AND knowledge_base_id = ? ORDER BY id LIMIT 1`, pageID, claim.Text, s.scope(ctx)).Scan(&claimID)
				if err == sql.ErrNoRows {
					claimID = newID("claim", runID+pageID+claim.Text)
					claimType := claim.ClaimType
					if claimType == "" {
						claimType = domain.ClaimFact
					}
					_, err = tx.ExecContext(ctx, `INSERT INTO wiki_claims (id, knowledge_base_id, page_id, text, claim_type, status, created_by_run_id, updated_by_run_id) VALUES (?, ?, ?, ?, ?, 'active', ?, ?)`, claimID, s.scope(ctx), pageID, claim.Text, claimType, runID, runID)
					if err == nil {
						diff.ClaimsAdded++
						changed = true
					}
				} else if err != nil {
					return Diff{}, err
				}
				if err == nil {
					for _, chunkID := range claim.EvidenceChunkIDs {
						if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO claim_evidence (claim_id, source_chunk_id, relation, knowledge_base_id) VALUES (?, ?, 'supports', ?)`, claimID, chunkID, s.scope(ctx)); err != nil {
							return Diff{}, err
						}
						changed = true
					}
				}
			case "REVISE", "SUPERSEDE":
				if _, err = tx.ExecContext(ctx, `UPDATE wiki_claims SET status = 'superseded', updated_by_run_id = ? WHERE id = ? AND knowledge_base_id = ?`, runID, targetID, s.scope(ctx)); err != nil {
					return Diff{}, err
				}
				claimType := claim.ClaimType
				if claimType == "" {
					claimType = domain.ClaimFact
				}
				newClaimID := newID("claim", runID+pageID+claim.Text)
				if _, err = tx.ExecContext(ctx, `INSERT INTO wiki_claims (id, knowledge_base_id, page_id, text, claim_type, status, created_by_run_id, updated_by_run_id) VALUES (?, ?, ?, ?, ?, 'active', ?, ?)`, newClaimID, s.scope(ctx), pageID, claim.Text, claimType, runID, runID); err != nil {
					return Diff{}, err
				}
				for _, chunkID := range claim.EvidenceChunkIDs {
					if _, err = tx.ExecContext(ctx, `INSERT INTO claim_evidence (claim_id, source_chunk_id, relation, knowledge_base_id) VALUES (?, ?, 'supports', ?)`, newClaimID, chunkID, s.scope(ctx)); err != nil {
						return Diff{}, err
					}
				}
				diff.ClaimsRevised++
				changed = true
			case "MARK_DISPUTED":
				if _, err = tx.ExecContext(ctx, `UPDATE wiki_claims SET status = 'disputed', updated_by_run_id = ? WHERE id = ? AND knowledge_base_id = ?`, runID, targetID, s.scope(ctx)); err != nil {
					return Diff{}, err
				}
				for _, chunkID := range claim.EvidenceChunkIDs {
					if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO claim_evidence (claim_id, source_chunk_id, relation, knowledge_base_id) VALUES (?, ?, 'disputes', ?)`, targetID, chunkID, s.scope(ctx)); err != nil {
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
			result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id, knowledge_base_id) VALUES (?, ?, ?, ?, ?)`, sourceID, pageID, action.Relation, runID, s.scope(ctx))
			if err != nil {
				return Diff{}, err
			}
			if n, _ := result.RowsAffected(); n > 0 {
				diff.LinksAdded++
				changed = true
			}
		}
		if action.Action == domain.ActionMerge {
			sourcePage, sourceErr := loadPageTx(ctx, tx, sourceID, s.scope(ctx))
			if sourceErr != nil {
				return Diff{}, sourceErr
			}
			if _, err = tx.ExecContext(ctx, `UPDATE wiki_claims SET page_id = ?, section_id = NULL, updated_by_run_id = ? WHERE page_id = ? AND knowledge_base_id = ?`, pageID, runID, sourceID, s.scope(ctx)); err != nil {
				return Diff{}, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id, knowledge_base_id)
				SELECT CASE WHEN source_page_id = ? THEN ? ELSE source_page_id END,
				       CASE WHEN target_page_id = ? THEN ? ELSE target_page_id END,
				       relation, ?, knowledge_base_id
				FROM wiki_links
				WHERE knowledge_base_id = ? AND (source_page_id = ? OR target_page_id = ?)
				  AND CASE WHEN source_page_id = ? THEN ? ELSE source_page_id END != CASE WHEN target_page_id = ? THEN ? ELSE target_page_id END`,
				sourceID, pageID, sourceID, pageID, runID,
				s.scope(ctx), sourceID, sourceID,
				sourceID, pageID, sourceID, pageID); err != nil {
				return Diff{}, err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM wiki_links WHERE knowledge_base_id = ? AND (source_page_id = ? OR target_page_id = ?)`, s.scope(ctx), sourceID, sourceID); err != nil {
				return Diff{}, err
			}
			sourcePage.Status = domain.PageStatusMerged
			sourcePage.CurrentRevision++
			sourcePage.UpdatedAt = time.Now().UTC()
			if _, err = tx.ExecContext(ctx, `UPDATE wiki_pages SET status = ?, current_revision = ?, updated_at = ? WHERE id = ? AND knowledge_base_id = ?`, sourcePage.Status, sourcePage.CurrentRevision, sourcePage.UpdatedAt.Format(time.RFC3339Nano), sourceID, s.scope(ctx)); err != nil {
				return Diff{}, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id, knowledge_base_id) VALUES (?, ?, 'merged_into', ?, ?)`, sourceID, pageID, runID, s.scope(ctx)); err != nil {
				return Diff{}, err
			}
			diff.LinksAdded++
			snapshot, snapshotErr := snapshotTx(ctx, tx, sourcePage, s.scope(ctx))
			if snapshotErr != nil {
				return Diff{}, snapshotErr
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO wiki_revisions (page_id, revision_number, compilation_run_id, snapshot_json, change_summary, created_at, knowledge_base_id) VALUES (?, ?, ?, ?, ?, ?, ?)`, sourcePage.ID, sourcePage.CurrentRevision, runID, string(snapshot), action.Reason, sourcePage.UpdatedAt.Format(time.RFC3339Nano), s.scope(ctx)); err != nil {
				return Diff{}, err
			}
			diff.Revisions++
			diff.Pages = append(diff.Pages, sourcePage.ID)
			changed = true
		}
		if changed {
			page.CurrentRevision++
			page.UpdatedAt = time.Now().UTC()
			if _, err = tx.ExecContext(ctx, `UPDATE wiki_pages SET title = ?, summary = ?, current_revision = ?, updated_at = ? WHERE id = ? AND knowledge_base_id = ?`, page.Title, page.Summary, page.CurrentRevision, page.UpdatedAt.Format(time.RFC3339Nano), page.ID, s.scope(ctx)); err != nil {
				return Diff{}, err
			}
			snapshot, err := snapshotTx(ctx, tx, page, s.scope(ctx))
			if err != nil {
				return Diff{}, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO wiki_revisions (page_id, revision_number, compilation_run_id, snapshot_json, change_summary, created_at, knowledge_base_id) VALUES (?, ?, ?, ?, ?, ?, ?)`, page.ID, page.CurrentRevision, runID, string(snapshot), action.Reason, page.UpdatedAt.Format(time.RFC3339Nano), s.scope(ctx)); err != nil {
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
	result, err := tx.ExecContext(ctx, `UPDATE compilation_runs SET status = ?, apply_result_json = ?, diff_json = ?, error = NULL WHERE id = ? AND knowledge_base_id = ?`, RunRenderPending, `{"applied":true,"rendered":false}`, string(diffRaw), runID, s.scope(ctx))
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

func loadPageTx(ctx context.Context, tx *sql.Tx, id, knowledgeBaseID string) (domain.WikiPage, error) {
	var page domain.WikiPage
	var created, updated string
	err := tx.QueryRowContext(ctx, `SELECT id, slug, page_type, title, summary, status, current_revision, created_at, updated_at FROM wiki_pages WHERE id = ? AND knowledge_base_id = ?`, id, knowledgeBaseID).Scan(&page.ID, &page.Slug, &page.PageType, &page.Title, &page.Summary, &page.Status, &page.CurrentRevision, &created, &updated)
	if err == nil {
		page.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		page.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	}
	return page, err
}

func targetClaimID(ctx context.Context, tx *sql.Tx, pageID string, claim domain.ClaimAction, knowledgeBaseID string) (string, error) {
	if claim.TargetClaimID != "" {
		return claim.TargetClaimID, nil
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM wiki_claims WHERE page_id = ? AND text = ? AND knowledge_base_id = ? ORDER BY id LIMIT 1`, pageID, claim.PreviousText, knowledgeBaseID).Scan(&id)
	return id, err
}

func snapshotTx(ctx context.Context, tx *sql.Tx, page domain.WikiPage, knowledgeBaseID string) ([]byte, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, page_id, COALESCE(section_id, ''), text, claim_type, status, created_by_run_id, updated_by_run_id FROM wiki_claims WHERE page_id = ? AND knowledge_base_id = ? ORDER BY id`, page.ID, knowledgeBaseID)
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
	rows, err := s.DB.QueryContext(ctx, `SELECT id, slug, title, page_type, summary FROM wiki_pages WHERE status = 'active' AND knowledge_base_id = ? ORDER BY slug`, s.scope(ctx))
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
		claimRows, err := s.DB.QueryContext(ctx, `SELECT text FROM wiki_claims WHERE page_id = ? AND status = 'active' AND knowledge_base_id = ? ORDER BY id`, pages[i].PageID, s.scope(ctx))
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
		return fmt.Errorf("invalid analysis: field=%q reason=%q document_id=%q summary_present=%t topic_count=%d", missingAnalysisField(analysis), "required field is missing", analysis.DocumentID, strings.TrimSpace(analysis.Summary) != "", len(analysis.Topics))
	}
	valid := map[string]bool{}
	for _, chunk := range chunks {
		valid[chunk.ID] = true
	}
	topicKeys := map[string]bool{}
	topicSlugs := map[string]bool{}
	for topicIndex, topic := range analysis.Topics {
		if strings.TrimSpace(topic.Key) == "" {
			return fmt.Errorf("invalid topic: index=%d key=%q title=%q slug=%q page_type=%q field=%q reason=%q", topicIndex, topic.Key, diagnosticText(topic.Title), topic.Slug, topic.PageType, "key", "must be non-empty")
		}
		if strings.TrimSpace(topic.Title) == "" {
			return fmt.Errorf("invalid topic: index=%d key=%q title=%q slug=%q page_type=%q field=%q reason=%q", topicIndex, topic.Key, diagnosticText(topic.Title), topic.Slug, topic.PageType, "title", "must be non-empty")
		}
		if strings.TrimSpace(topic.Slug) == "" {
			return fmt.Errorf("invalid topic: index=%d key=%q title=%q slug=%q page_type=%q field=%q reason=%q", topicIndex, topic.Key, diagnosticText(topic.Title), topic.Slug, topic.PageType, "slug", "must be non-empty")
		}
		if strings.TrimSpace(topic.Summary) == "" {
			return fmt.Errorf("invalid topic: index=%d key=%q title=%q slug=%q page_type=%q field=%q reason=%q", topicIndex, topic.Key, diagnosticText(topic.Title), topic.Slug, topic.PageType, "summary", "must be non-empty")
		}
		if !validPageType(topic.PageType) {
			return fmt.Errorf("invalid topic: index=%d key=%q title=%q slug=%q page_type=%q field=%q value=%q constraint=%q reason=%q", topicIndex, topic.Key, diagnosticText(topic.Title), topic.Slug, topic.PageType, "page_type", topic.PageType, "one of concept, entity, topic", "unsupported page type")
		}
		if topicKeys[topic.Key] {
			return fmt.Errorf("invalid topic: index=%d key=%q title=%q slug=%q page_type=%q field=%q value=%q reason=%q", topicIndex, topic.Key, diagnosticText(topic.Title), topic.Slug, topic.PageType, "key", topic.Key, "must be unique within analysis")
		}
		if topicSlugs[topic.Slug] {
			return fmt.Errorf("invalid topic: index=%d key=%q title=%q slug=%q page_type=%q field=%q value=%q reason=%q", topicIndex, topic.Key, diagnosticText(topic.Title), topic.Slug, topic.PageType, "slug", topic.Slug, "must be unique within analysis")
		}
		topicKeys[topic.Key] = true
		topicSlugs[topic.Slug] = true
		for claimIndex, claim := range topic.Claims {
			claimSummary := diagnosticText(claim.Text)
			evidence := fmt.Sprintf("%v", claim.EvidenceChunkIDs)
			claimPrefix := fmt.Sprintf("invalid claim: index=%d topic_key=%q claim_type=%q text=%q evidence_chunk_ids=%s", claimIndex, topic.Key, claim.ClaimType, claimSummary, evidence)
			if strings.TrimSpace(claim.Text) == "" {
				return fmt.Errorf("%s field=%q reason=%q", claimPrefix, "text", "must be non-empty")
			}
			if !validClaimType(claim.ClaimType) {
				return fmt.Errorf("%s field=%q value=%q constraint=%q reason=%q", claimPrefix, "claim_type", claim.ClaimType, "one of fact, definition, argument, procedure, caveat", "unsupported claim type")
			}
			if len(claim.EvidenceChunkIDs) == 0 {
				return fmt.Errorf("%s field=%q reason=%q", claimPrefix, "evidence_chunk_ids", "must contain at least one source chunk ID")
			}
			for _, id := range claim.EvidenceChunkIDs {
				if !valid[id] {
					return fmt.Errorf("%s field=%q value=%q reason=%q", claimPrefix, "evidence_chunk_ids", id, "source chunk does not exist in this document")
				}
			}
		}
	}
	for relationIndex, relation := range analysis.Relations {
		relationPrefix := fmt.Sprintf("invalid relation: index=%d source_topic_key=%q target_topic_key=%q relation=%q", relationIndex, relation.SourceTopicKey, relation.TargetTopicKey, relation.Relation)
		if strings.TrimSpace(relation.SourceTopicKey) == "" {
			return fmt.Errorf("%s field=%q reason=%q", relationPrefix, "source_topic_key", "must be non-empty")
		}
		if strings.TrimSpace(relation.TargetTopicKey) == "" {
			return fmt.Errorf("%s field=%q reason=%q", relationPrefix, "target_topic_key", "must be non-empty")
		}
		if relation.SourceTopicKey == relation.TargetTopicKey {
			return fmt.Errorf("%s field=%q value=%q reason=%q", relationPrefix, "target_topic_key", relation.TargetTopicKey, "must differ from source_topic_key")
		}
		if !topicKeys[relation.SourceTopicKey] {
			return fmt.Errorf("%s field=%q value=%q reason=%q", relationPrefix, "source_topic_key", relation.SourceTopicKey, "must reference a topic in this analysis")
		}
		if !topicKeys[relation.TargetTopicKey] {
			return fmt.Errorf("%s field=%q value=%q reason=%q", relationPrefix, "target_topic_key", relation.TargetTopicKey, "must reference a topic in this analysis")
		}
		if !validRelation(relation.Relation) {
			return fmt.Errorf("%s field=%q value=%q constraint=%q reason=%q", relationPrefix, "relation", relation.Relation, "one of related_to, part_of, depends_on, contradicts, supports, references", "unsupported relation")
		}
	}
	return nil
}

func missingAnalysisField(analysis domain.SourceAnalysis) string {
	if analysis.DocumentID == "" {
		return "document_id"
	}
	if strings.TrimSpace(analysis.Summary) == "" {
		return "summary"
	}
	return "topics"
}

func diagnosticText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	const limit = 160
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "..."
	}
	return value
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

type relatePage struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	PageType string `json:"page_type"`
	Summary  string `json:"summary"`
}

// RelateAll asks the model to relate every active Wiki page to the others and
// stores confirmed relations as wiki_links attributed to runID, then re-renders
// the projection. It is best-effort: it never alters compiled claims, so callers
// (the compile pipeline, the relink endpoint) may log a failure and continue.
// It returns the number of newly created links.
func (s Service) RelateAll(ctx context.Context, runID string) (int, error) {
	client := s.LLM
	if client == nil {
		return 0, errors.New("compiler LLM is not configured")
	}
	pages, claims, _, err := s.loadWikiState(ctx)
	if err != nil {
		return 0, err
	}
	if len(pages) < 2 {
		return 0, nil
	}
	claimsByPage := map[string][]string{}
	for _, claim := range claims {
		if claim.Status == "active" || claim.Status == "disputed" {
			claimsByPage[claim.PageID] = append(claimsByPage[claim.PageID], claim.Text)
		}
	}
	active := make(map[string]bool, len(pages))
	catalog := make([]relatePage, 0, len(pages))
	for _, page := range pages {
		active[page.ID] = true
		summary := page.Summary
		if extra := claimsByPage[page.ID]; len(extra) > 0 {
			summary += " " + strings.Join(extra, " ")
		}
		catalog = append(catalog, relatePage{ID: page.ID, Slug: page.Slug, Title: page.Title, PageType: string(page.PageType), Summary: summary})
	}
	pagesRaw, _ := json.Marshal(catalog)
	raw, err := client.SuggestLinks(ctx, llm.LinkInput{Pages: pagesRaw, Catalog: json.RawMessage("[]")})
	if err != nil {
		return 0, err
	}
	suggestions, err := llm.DecodeStrict[domain.LinkSuggestions](raw)
	if err != nil {
		return 0, err
	}
	existing, err := s.loadWikiLinks(ctx)
	if err != nil {
		return 0, err
	}
	seen := make(map[string]bool, len(existing))
	for _, link := range existing {
		seen[link.SourcePageID+"\x00"+link.TargetPageID] = true
	}
	added := 0
	for _, suggestion := range suggestions.Links {
		if suggestion.SourcePageID == suggestion.TargetPageID || !active[suggestion.SourcePageID] || !active[suggestion.TargetPageID] || !validRelation(suggestion.Relation) {
			continue
		}
		key := suggestion.SourcePageID + "\x00" + suggestion.TargetPageID
		if seen[key] {
			continue
		}
		result, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id, knowledge_base_id) VALUES (?, ?, ?, ?, ?)`, suggestion.SourcePageID, suggestion.TargetPageID, suggestion.Relation, runID, s.scope(ctx))
		if err != nil {
			return added, err
		}
		seen[key] = true
		if n, _ := result.RowsAffected(); n > 0 {
			added++
		}
	}
	if added > 0 {
		if err := s.renderDefaultProjection(ctx); err != nil {
			return added, err
		}
	}
	return added, nil
}

// Relink rebuilds the cross-page link graph over all active pages on demand,
// attributing new links to the most recent compilation run. It backfills the
// graph without recompiling each document.
func (s Service) Relink(ctx context.Context) (int, error) {
	var runID string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM compilation_runs WHERE knowledge_base_id = ? ORDER BY created_at DESC LIMIT 1`, s.scope(ctx)).Scan(&runID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	related, err := s.RelateAll(ctx, runID)
	if err != nil {
		return related, err
	}
	mentioned, err := s.linkMentions(ctx, runID)
	return related + mentioned, err
}

// linkMentions is the deterministic backstop for missing links. It scans every
// active page's compiled text (summary and claims) for exact mentions of other
// active pages' titles and records a `references` wiki_link for each. Because the
// link is stored as data — not merely rendered as a [[..]] at display time — it
// feeds retrieval's 1-hop link expansion, the graph, and backlinks, so a named
// subject that owns its own page is reachable in search wherever it is mentioned,
// even when the LLM relate step never proposed the relation. Pairs already
// connected in either direction keep their more specific relation and are skipped.
// It re-renders the projection when it adds links and returns the number added.
func (s Service) linkMentions(ctx context.Context, runID string) (int, error) {
	pages, claims, _, err := s.loadWikiState(ctx)
	if err != nil {
		return 0, err
	}
	if len(pages) < 2 {
		return 0, nil
	}
	textByPage := make(map[string]string, len(pages))
	for _, page := range pages {
		textByPage[page.ID] = page.Summary
	}
	for _, claim := range claims {
		if claim.Status == "active" || claim.Status == "disputed" {
			textByPage[claim.PageID] += "\n" + claim.Text
		}
	}
	existing, err := s.loadWikiLinks(ctx)
	if err != nil {
		return 0, err
	}
	connected := make(map[string]bool, len(existing))
	pairKey := func(a, b string) string { return a + "\x00" + b }
	for _, link := range existing {
		connected[pairKey(link.SourcePageID, link.TargetPageID)] = true
		connected[pairKey(link.TargetPageID, link.SourcePageID)] = true
	}
	// Match longer titles first so a title that contains a shorter one resolves to
	// the most specific page and nested titles are not double-counted.
	ordered := append([]domain.WikiPage(nil), pages...)
	sort.Slice(ordered, func(i, j int) bool { return len([]rune(ordered[i].Title)) > len([]rune(ordered[j].Title)) })
	added := 0
	for _, source := range pages {
		text := textByPage[source.ID]
		if text == "" {
			continue
		}
		for _, target := range ordered {
			if target.ID == source.ID || target.Title == "" {
				continue
			}
			if connected[pairKey(source.ID, target.ID)] || connected[pairKey(target.ID, source.ID)] {
				continue
			}
			if !strings.Contains(text, target.Title) {
				continue
			}
			result, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id, knowledge_base_id) VALUES (?, ?, 'references', ?, ?)`, source.ID, target.ID, runID, s.scope(ctx))
			if err != nil {
				return added, err
			}
			connected[pairKey(source.ID, target.ID)] = true
			connected[pairKey(target.ID, source.ID)] = true
			if n, _ := result.RowsAffected(); n > 0 {
				added++
			}
		}
	}
	if added > 0 {
		if err := s.renderDefaultProjection(ctx); err != nil {
			return added, err
		}
	}
	return added, nil
}

// DeleteSummary reports what a source deletion removed.
type DeleteSummary struct {
	DeletedPages  int `json:"deleted_pages"`
	DeletedClaims int `json:"deleted_claims"`
}

type BatchDeleteSummary struct {
	DeletedSources int `json:"deleted_sources"`
	DeletedPages   int `json:"deleted_pages"`
	DeletedClaims  int `json:"deleted_claims"`
}

func inClause(ids []string) (string, []any) {
	if len(ids) == 0 {
		// A non-empty sentinel that no real id equals: "x IN ('')" matches nothing
		// and "x NOT IN ('')" is true for every real id (unlike NULL, which is unknown).
		return "('')", nil
	}
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i] = "?"
		args[i] = id
	}
	return "(" + strings.Join(marks, ",") + ")", args
}

func scanIDsTx(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PLACEHOLDER_DELETE_METHODS

// DeleteSource removes a source and every page derived exclusively from it,
// preserving pages that other sources still support. It deletes the source's
// original and parsed files, rebuilds the Wiki projection, reindexes, and
// rebuilds cross-page links. Returns sql.ErrNoRows if the source is unknown.
func (s Service) DeleteSource(ctx context.Context, documentID string) (DeleteSummary, error) {
	summary, err := s.deleteSourceRows(ctx, documentID)
	if err != nil {
		return DeleteSummary{}, err
	}
	s.rebuildAfterDelete(ctx)
	return summary, nil
}

// DeleteSources removes several sources in one call and rebuilds the Wiki
// projection, index, and links only once afterward instead of per source.
// Sources are deleted in order; on the first failure it stops, rebuilds for
// whatever committed, and returns the accumulated counts alongside the error.
// Returns sql.ErrNoRows if the first unknown source is reached before any
// deletion succeeds.
func (s Service) DeleteSources(ctx context.Context, documentIDs []string) (BatchDeleteSummary, error) {
	if len(documentIDs) == 0 {
		return BatchDeleteSummary{}, fmt.Errorf("no source ids provided")
	}
	summary := BatchDeleteSummary{}
	var failure error
	for _, documentID := range documentIDs {
		rows, err := s.deleteSourceRows(ctx, documentID)
		if err != nil {
			failure = err
			break
		}
		summary.DeletedSources++
		summary.DeletedPages += rows.DeletedPages
		summary.DeletedClaims += rows.DeletedClaims
	}
	if summary.DeletedSources > 0 {
		s.rebuildAfterDelete(ctx)
	}
	return summary, failure
}

// deleteSourceRows performs the transactional deletion of a single source and
// its exclusively-derived Wiki rows, plus the source's on-disk files. It does
// not re-render, reindex, or relink; callers do that once via rebuildAfterDelete
// so batch deletes pay the rebuild cost a single time.
func (s Service) deleteSourceRows(ctx context.Context, documentID string) (DeleteSummary, error) {
	scope := s.scope(ctx)
	var originalPath, parsedPath string
	if err := s.DB.QueryRowContext(ctx, `SELECT original_path, COALESCE(parsed_path, '') FROM source_documents WHERE id = ? AND knowledge_base_id = ?`, documentID, scope).Scan(&originalPath, &parsedPath); err != nil {
		return DeleteSummary{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return DeleteSummary{}, err
	}
	defer tx.Rollback()

	chunks, err := scanIDsTx(ctx, tx, `SELECT id FROM source_chunks WHERE document_id = ? AND knowledge_base_id = ?`, documentID, scope)
	if err != nil {
		return DeleteSummary{}, err
	}
	runs, err := scanIDsTx(ctx, tx, `SELECT id FROM compilation_runs WHERE document_id = ? AND knowledge_base_id = ?`, documentID, scope)
	if err != nil {
		return DeleteSummary{}, err
	}
	chunkIn, chunkArgs := inClause(chunks)
	citing, err := scanIDsTx(ctx, tx, `SELECT DISTINCT claim_id FROM claim_evidence WHERE knowledge_base_id = ? AND source_chunk_id IN `+chunkIn, append([]any{scope}, chunkArgs...)...)
	if err != nil {
		return DeleteSummary{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM claim_evidence WHERE knowledge_base_id = ? AND source_chunk_id IN `+chunkIn, append([]any{scope}, chunkArgs...)...); err != nil {
		return DeleteSummary{}, err
	}
	citingIn, citingArgs := inClause(citing)
	claimsToDelete, err := scanIDsTx(ctx, tx, `SELECT c.id FROM wiki_claims c LEFT JOIN claim_evidence ce ON ce.claim_id = c.id AND ce.knowledge_base_id = c.knowledge_base_id WHERE c.knowledge_base_id = ? AND c.id IN `+citingIn+` GROUP BY c.id HAVING COUNT(ce.source_chunk_id) = 0`, append([]any{scope}, citingArgs...)...)
	if err != nil {
		return DeleteSummary{}, err
	}
	deleteSet := make(map[string]bool, len(claimsToDelete))
	for _, id := range claimsToDelete {
		deleteSet[id] = true
	}
	// pagesToDelete: pages touched by this source whose every claim is being deleted.
	touchedPages, err := scanIDsTx(ctx, tx, `SELECT DISTINCT page_id FROM wiki_claims WHERE knowledge_base_id = ? AND id IN `+citingIn, append([]any{scope}, citingArgs...)...)
	if err != nil {
		return DeleteSummary{}, err
	}
	pagesToDelete := make([]string, 0, len(touchedPages))
	for _, pageID := range touchedPages {
		claimIDs, err := scanIDsTx(ctx, tx, `SELECT id FROM wiki_claims WHERE page_id = ? AND knowledge_base_id = ?`, pageID, scope)
		if err != nil {
			return DeleteSummary{}, err
		}
		survivor := false
		for _, id := range claimIDs {
			if !deleteSet[id] {
				survivor = true
				break
			}
		}
		if !survivor && len(claimIDs) > 0 {
			pagesToDelete = append(pagesToDelete, pageID)
		}
	}
	runIn, runArgs := inClause(runs)
	delIn, delArgs := inClause(claimsToDelete)
	pageIn, pageArgs := inClause(pagesToDelete)
	// Repoint surviving claims that this source's runs last touched, so deleting
	// those runs does not violate the updated_by_run_id foreign key. Match by id
	// sets (globally unique) rather than scope, since a row's stored
	// knowledge_base_id may differ from the request scope.
	if _, err := tx.ExecContext(ctx, `UPDATE wiki_claims SET updated_by_run_id = created_by_run_id WHERE updated_by_run_id IN `+runIn+` AND id NOT IN `+delIn, concatArgs(runArgs, delArgs)...); err != nil {
		return DeleteSummary{}, err
	}
	steps := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM claim_evidence WHERE claim_id IN ` + delIn, delArgs},
		{`DELETE FROM wiki_links WHERE source_page_id IN ` + pageIn + ` OR target_page_id IN ` + pageIn + ` OR created_by_run_id IN ` + runIn, concatArgs(pageArgs, pageArgs, runArgs)},
		{`DELETE FROM wiki_revisions WHERE page_id IN ` + pageIn + ` OR compilation_run_id IN ` + runIn, concatArgs(pageArgs, runArgs)},
		{`DELETE FROM wiki_claims WHERE id IN ` + delIn, delArgs},
		{`DELETE FROM wiki_sections WHERE page_id IN ` + pageIn, pageArgs},
		{`DELETE FROM wiki_passages WHERE page_id IN ` + pageIn, pageArgs},
		{`DELETE FROM wiki_fts WHERE page_id IN ` + pageIn, pageArgs},
		{`DELETE FROM wiki_pages WHERE id IN ` + pageIn, pageArgs},
		{`DELETE FROM source_chunk_embeddings WHERE source_chunk_id IN ` + chunkIn, chunkArgs},
		{`DELETE FROM source_chunks WHERE document_id = ?`, []any{documentID}},
		{`DELETE FROM compilation_runs WHERE id IN ` + runIn, runArgs},
		{`DELETE FROM source_documents WHERE id = ?`, []any{documentID}},
	}
	for _, step := range steps {
		if _, err := tx.ExecContext(ctx, step.query, step.args...); err != nil {
			return DeleteSummary{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return DeleteSummary{}, err
	}
	if originalPath != "" {
		_ = os.Remove(originalPath)
	}
	if parsedPath != "" {
		_ = os.Remove(parsedPath)
	}
	return DeleteSummary{DeletedPages: len(pagesToDelete), DeletedClaims: len(claimsToDelete)}, nil
}

// rebuildAfterDelete re-renders the Markdown projection, reindexes, and rebuilds
// cross-page links after one or more sources have been removed. Failures are
// logged but not fatal: the deletion already committed.
func (s Service) rebuildAfterDelete(ctx context.Context) {
	if err := s.renderDefaultProjection(ctx); err != nil {
		logging.Logger(ctx).Error("re-render after source delete failed", "error", logging.SafeSummary(err))
	}
	if s.Index != nil {
		if _, err := s.Index.Reindex(ctx); err != nil {
			logging.Logger(ctx).Error("reindex after source delete failed", "error", logging.SafeSummary(err))
		}
	}
	if _, err := s.Relink(ctx); err != nil {
		logging.Logger(ctx).Error("relink after source delete failed", "error", logging.SafeSummary(err))
	}
}

func concatArgs(groups ...[]any) []any {
	out := make([]any, 0)
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

// DeletePage removes a single Wiki page (by slug or id) and everything anchored
// to it — its claims, evidence, sections, revisions, and any link touching it —
// then rebuilds the projection and index. The source and its chunks are kept.
// Returns sql.ErrNoRows if the page is unknown.
func (s Service) DeletePage(ctx context.Context, key string) error {
	scope := s.scope(ctx)
	var pageID string
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM wiki_pages WHERE (slug = ? OR id = ?) AND knowledge_base_id = ?`, key, key, scope).Scan(&pageID); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	steps := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM claim_evidence WHERE claim_id IN (SELECT id FROM wiki_claims WHERE page_id = ?)`, []any{pageID}},
		{`DELETE FROM wiki_links WHERE source_page_id = ? OR target_page_id = ?`, []any{pageID, pageID}},
		{`DELETE FROM wiki_revisions WHERE page_id = ?`, []any{pageID}},
		{`DELETE FROM wiki_claims WHERE page_id = ?`, []any{pageID}},
		{`DELETE FROM wiki_sections WHERE page_id = ?`, []any{pageID}},
		{`DELETE FROM wiki_passages WHERE page_id = ?`, []any{pageID}},
		{`DELETE FROM wiki_fts WHERE page_id = ?`, []any{pageID}},
		{`DELETE FROM wiki_pages WHERE id = ?`, []any{pageID}},
	}
	for _, step := range steps {
		if _, err := tx.ExecContext(ctx, step.query, step.args...); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := s.renderDefaultProjection(ctx); err != nil {
		logging.Logger(ctx).Error("re-render after page delete failed", "page_id", pageID, "error", logging.SafeSummary(err))
	}
	if s.Index != nil {
		if _, err := s.Index.Reindex(ctx); err != nil {
			logging.Logger(ctx).Error("reindex after page delete failed", "page_id", pageID, "error", logging.SafeSummary(err))
		}
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
	err := s.DB.QueryRowContext(ctx, `SELECT id, original_name, media_type, sha256, original_path, COALESCE(parsed_path, ''), status, parser_version, created_at, COALESCE(parse_error, '') FROM source_documents WHERE id = ? AND knowledge_base_id = ?`, id, s.scope(ctx)).Scan(&d.ID, &d.OriginalName, &d.MediaType, &d.SHA256, &d.OriginalPath, &d.ParsedPath, &d.Status, &d.ParserVersion, &created, &d.ParseError)
	if err != nil {
		return d, err
	}
	d.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return d, err
}

func (s Service) loadChunks(ctx context.Context, id string) ([]domain.SourceChunk, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, document_id, chunk_index, text, page_number, heading_path, char_start, char_end, content_hash FROM source_chunks WHERE document_id = ? AND knowledge_base_id = ? ORDER BY chunk_index`, id, s.scope(ctx))
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
	pageRows, err := s.DB.QueryContext(ctx, `SELECT id, slug, page_type, title, summary, status, current_revision, created_at, updated_at FROM wiki_pages WHERE status = 'active' AND knowledge_base_id = ? ORDER BY slug`, s.scope(ctx))
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
	claimRows, err := s.DB.QueryContext(ctx, `SELECT c.id, c.page_id, COALESCE(c.section_id, ''), c.text, c.claim_type, c.status, c.created_by_run_id, c.updated_by_run_id FROM wiki_claims c JOIN wiki_pages p ON p.id = c.page_id WHERE p.status = 'active' AND c.knowledge_base_id = ? AND p.knowledge_base_id = ? ORDER BY c.page_id, c.id`, s.scope(ctx), s.scope(ctx))
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
	evidenceRows, err := s.DB.QueryContext(ctx, `SELECT ce.claim_id, ce.source_chunk_id, ce.relation, COALESCE(ce.note, '') FROM claim_evidence ce JOIN wiki_claims c ON c.id = ce.claim_id JOIN wiki_pages p ON p.id = c.page_id WHERE p.status = 'active' AND ce.knowledge_base_id = ? AND c.knowledge_base_id = ? AND p.knowledge_base_id = ? ORDER BY ce.claim_id, ce.source_chunk_id`, s.scope(ctx), s.scope(ctx), s.scope(ctx))
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
	rows, err := s.DB.QueryContext(ctx, `SELECT l.source_page_id, l.target_page_id, l.relation, l.created_by_run_id FROM wiki_links l JOIN wiki_pages sp ON sp.id = l.source_page_id AND sp.status = 'active' JOIN wiki_pages tp ON tp.id = l.target_page_id AND tp.status = 'active' WHERE l.knowledge_base_id = ? AND sp.knowledge_base_id = ? AND tp.knowledge_base_id = ? ORDER BY l.source_page_id, l.target_page_id, l.relation`, s.scope(ctx), s.scope(ctx), s.scope(ctx))
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

// pluralType is the directory name for a page type's Markdown projection.
// Naive "+s" would misspell entity as "entitys".
func pluralType(pageType domain.PageType) string {
	if pageType == domain.PageTypeEntity {
		return "entities"
	}
	return string(pageType) + "s"
}

type pageRef struct{ slug, title string }

// linkifyMarkdown rewrites mentions of connected page titles in prose into
// Obsidian-style [[slug|title]] links, preferring the longest title at each
// position so nested titles resolve to the most specific page.
func linkifyMarkdown(text string, conns []pageRef) string {
	if text == "" || len(conns) == 0 {
		return text
	}
	ordered := append([]pageRef(nil), conns...)
	sort.Slice(ordered, func(i, j int) bool { return len([]rune(ordered[i].title)) > len([]rune(ordered[j].title)) })
	runes := []rune(text)
	var b strings.Builder
	for i := 0; i < len(runes); {
		matched := false
		for _, c := range ordered {
			title := []rune(c.title)
			if len(title) == 0 || i+len(title) > len(runes) {
				continue
			}
			if string(runes[i:i+len(title)]) == c.title {
				fmt.Fprintf(&b, "[[%s|%s]]", c.slug, c.title)
				i += len(title)
				matched = true
				break
			}
		}
		if !matched {
			b.WriteRune(runes[i])
			i++
		}
	}
	return b.String()
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
	// Connected pages per page (both directions) drive inline [[slug|title]] links
	// woven into the prose, so a mention like "暖橙面包店" is a link in the body.
	// The edges come from wiki_links, which linkMentions backfills for plain title
	// mentions, so display and the searchable link graph stay in sync.
	connectionsByPage := map[string][]pageRef{}
	seenConn := map[string]map[string]bool{}
	addConn := func(pageID, otherID string) {
		other, ok := pageByID[otherID]
		if !ok || otherID == pageID || other.Title == "" {
			return
		}
		if seenConn[pageID] == nil {
			seenConn[pageID] = map[string]bool{}
		}
		if seenConn[pageID][otherID] {
			return
		}
		seenConn[pageID][otherID] = true
		connectionsByPage[pageID] = append(connectionsByPage[pageID], pageRef{slug: other.Slug, title: other.Title})
	}
	for _, link := range links {
		if _, ok := pageByID[link.SourcePageID]; !ok {
			continue
		}
		if _, ok := pageByID[link.TargetPageID]; !ok {
			continue
		}
		addConn(link.SourcePageID, link.TargetPageID)
		addConn(link.TargetPageID, link.SourcePageID)
	}
	for _, claim := range claims {
		claimByPage[claim.PageID] = append(claimByPage[claim.PageID], claim)
	}
	for _, item := range evidence {
		evidenceByClaim[item.ClaimID] = append(evidenceByClaim[item.ClaimID], item)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].Slug < pages[j].Slug })
	for _, pageType := range []domain.PageType{domain.PageTypeConcept, domain.PageTypeEntity, domain.PageTypeTopic} {
		if err := os.MkdirAll(filepath.Join(outputDir, pluralType(pageType)), 0o755); err != nil {
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
		fmt.Fprintf(&index, "- [%s](%s/%s.md)\n", page.Title, pluralType(page.PageType), page.Slug)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "index.md"), []byte(index.String()), 0o644); err != nil {
		return err
	}
	for _, page := range pages {
		if !validRenderSlug(page.Slug) {
			return fmt.Errorf("refuse to render wiki page with unsafe slug %q", page.Slug)
		}
		var b strings.Builder
		conns := connectionsByPage[page.ID]
		fmt.Fprintf(&b, "---\nslug: %s\npage_type: %s\nstatus: %s\nrevision: %d\n---\n\n# %s\n\n%s\n\n## Claims\n", page.Slug, page.PageType, page.Status, page.CurrentRevision, page.Title, linkifyMarkdown(page.Summary, conns))
		items := claimByPage[page.ID]
		sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
		for _, claim := range items {
			if claim.Status != "active" && claim.Status != "disputed" {
				continue
			}
			fmt.Fprintf(&b, "\n- **%s**: %s", claim.ClaimType, linkifyMarkdown(claim.Text, conns))
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
			filepath.Join(outputDir, pluralType(page.PageType), page.Slug+".md"),
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
