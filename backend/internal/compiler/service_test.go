package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	appdb "github.com/joeychen/llm-wiki-demo/backend/internal/db"
	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	"github.com/joeychen/llm-wiki-demo/backend/internal/sources"
)

func TestCompileCreatesThenUpdatesAndRendersWithEvidence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceService := sources.Service{DB: database, DataRoot: root}
	compilerService := Service{DB: database, DataRoot: root, LLM: llm.DeterministicFake{}}

	first, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "knowledge-a.md", Data: []byte("# Shared Concept\n\nThe first source establishes the baseline."), MediaType: "text/markdown"})
	if err != nil {
		t.Fatal(err)
	}
	firstRun, err := compilerService.Compile(ctx, first.Document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstRun.Status != RunApplied {
		t.Fatalf("first run = %#v", firstRun)
	}

	var pageID string
	if err := database.QueryRow(`SELECT id FROM wiki_pages WHERE slug = 'shared-concept'`).Scan(&pageID); err != nil {
		t.Fatal(err)
	}
	var revisions int
	if err := database.QueryRow(`SELECT current_revision FROM wiki_pages WHERE id = ?`, pageID).Scan(&revisions); err != nil {
		t.Fatal(err)
	}

	second, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "knowledge-b.md", Data: []byte("# Shared Concept\n\nThe second source adds corroborating evidence."), MediaType: "text/markdown"})
	if err != nil {
		t.Fatal(err)
	}
	secondRun, err := compilerService.Compile(ctx, second.Document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secondRun.Status != RunApplied {
		t.Fatalf("second run = %#v", secondRun)
	}

	var pageCount, claimCount, evidenceCount, sourceCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM wiki_pages WHERE slug = 'shared-concept'`).Scan(&pageCount); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM wiki_claims WHERE page_id = ?`, pageID).Scan(&claimCount); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM claim_evidence ce JOIN wiki_claims wc ON wc.id = ce.claim_id WHERE wc.page_id = ?`, pageID).Scan(&evidenceCount); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(DISTINCT sd.id) FROM source_documents sd JOIN source_chunks sc ON sc.document_id = sd.id JOIN claim_evidence ce ON ce.source_chunk_id = sc.id JOIN wiki_claims wc ON wc.id = ce.claim_id WHERE wc.page_id = ?`, pageID).Scan(&sourceCount); err != nil {
		t.Fatal(err)
	}
	if pageCount != 1 || claimCount != 2 || evidenceCount != 2 || sourceCount != 2 {
		t.Fatalf("page/claim/evidence/source counts = %d/%d/%d/%d", pageCount, claimCount, evidenceCount, sourceCount)
	}
	if err := database.QueryRow(`SELECT current_revision FROM wiki_pages WHERE id = ?`, pageID).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if revisions != 2 {
		t.Fatalf("revision = %d, want 2", revisions)
	}

	run, err := compilerService.GetRun(ctx, secondRun.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]json.RawMessage{"analyze": run.Analyze, "candidates": run.Candidates, "plan": run.Plan, "validation": run.Validation, "apply": run.ApplyResult, "diff": run.Diff} {
		if len(raw) == 0 {
			t.Errorf("run.%s is empty", name)
		}
	}
	var plan domain.CompilationPlan
	if err := json.Unmarshal(run.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.PageActions) != 1 || plan.PageActions[0].Action != domain.ActionUpdate || plan.PageActions[0].TargetPageID != pageID {
		t.Fatalf("second plan = %#v", plan)
	}

	markdown, err := os.ReadFile(filepath.Join(root, "wiki", "shared-concept.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(markdown)
	if !strings.Contains(text, "first source") || !strings.Contains(text, "second source") || !strings.Contains(text, "chunk_") {
		t.Fatalf("rendered markdown lacks accumulated claims/evidence: %s", text)
	}
}

func TestApplyRejectsInvalidPlanAndRollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceService := sources.Service{DB: database, DataRoot: root}
	source, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "validation.txt", MediaType: "text/plain", Data: []byte("evidence for validation")})
	if err != nil {
		t.Fatal(err)
	}
	service := Service{DB: database, DataRoot: root, LLM: llm.DeterministicFake{}}

	badAction := domain.CompilationPlan{DocumentID: source.Document.ID, SchemaVersion: "phase2-v1", PageActions: []domain.PageAction{{Action: "INVALID", Reason: "bad"}}}
	if err := service.ValidatePlan(ctx, badAction, source.Document.ID); err == nil {
		t.Fatal("invalid action was accepted")
	}
	badChunk := domain.CompilationPlan{DocumentID: source.Document.ID, SchemaVersion: "phase2-v1", PageActions: []domain.PageAction{{Action: domain.ActionCreate, Slug: "bad", PageType: domain.PageTypeConcept, Title: "Bad", Summary: "Bad", Reason: "bad", ClaimActions: []domain.ClaimAction{{Action: "ADD", Text: "bad", EvidenceChunkIDs: []string{"missing"}}}}}}
	if err := service.ValidatePlan(ctx, badChunk, source.Document.ID); err == nil {
		t.Fatal("missing evidence chunk was accepted")
	}

	runID := "run_rollback"
	if _, err := database.Exec(`INSERT INTO compilation_runs (id, document_id, status, created_at) VALUES (?, ?, 'validated', '2024-01-01T00:00:00Z')`, runID, source.Document.ID); err != nil {
		t.Fatal(err)
	}
	plan := domain.CompilationPlan{DocumentID: source.Document.ID, SchemaVersion: "phase2-v1", PageActions: []domain.PageAction{
		{Action: domain.ActionCreate, Slug: "rollback-one", PageType: domain.PageTypeConcept, Title: "One", Summary: "One", Reason: "one", ClaimActions: []domain.ClaimAction{{Action: "ADD", Text: "one", EvidenceChunkIDs: []string{source.Chunks[0].ID}}}},
		{Action: domain.ActionCreate, Slug: "rollback-two", PageType: domain.PageTypeConcept, Title: "Two", Summary: "Two", Reason: "two", ClaimActions: []domain.ClaimAction{{Action: "ADD", Text: "two", EvidenceChunkIDs: []string{source.Chunks[0].ID}}}},
	}}
	service.FailAfter = 1
	if _, err := service.ApplyPlan(ctx, runID, plan); err == nil {
		t.Fatal("injected apply failure was not returned")
	}
	for table := range map[string]bool{"wiki_pages": true, "wiki_claims": true, "claim_evidence": true, "wiki_revisions": true} {
		var count int
		if err := database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s has %d rows after rollback", table, count)
		}
	}
}

func TestNoOpDoesNotCreateRevision(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceService := sources.Service{DB: database, DataRoot: root}
	source, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "noop.txt", MediaType: "text/plain", Data: []byte("noop evidence")})
	if err != nil {
		t.Fatal(err)
	}
	service := Service{DB: database, DataRoot: root, LLM: llm.DeterministicFake{}}
	first, err := service.Compile(ctx, source.Document.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pageID string
	if err := database.QueryRow(`SELECT id FROM wiki_pages LIMIT 1`).Scan(&pageID); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := database.QueryRow(`SELECT COUNT(*) FROM wiki_revisions WHERE page_id = ?`, pageID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	plan := domain.CompilationPlan{DocumentID: source.Document.ID, SchemaVersion: "phase2-v1", PageActions: []domain.PageAction{{Action: domain.ActionNoOp, TargetPageID: pageID, Reason: "no new knowledge"}}}
	if _, err := service.ApplyPlan(ctx, first.RunID, plan); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := database.QueryRow(`SELECT COUNT(*) FROM wiki_revisions WHERE page_id = ?`, pageID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("NO_OP revisions changed from %d to %d", before, after)
	}
}

func TestLinkApplyCreatesWikiLink(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service := Service{DB: database, DataRoot: root}
	sourceService := sources.Service{DB: database, DataRoot: root}
	source, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "link.txt", MediaType: "text/plain", Data: []byte("evidence for linked concepts")})
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []struct{ id, slug, title string }{{"page_alpha", "alpha", "Alpha"}, {"page_beta", "beta", "Beta"}} {
		_, err = database.Exec(`INSERT INTO wiki_pages (id, slug, page_type, title, summary, status, current_revision, created_at, updated_at) VALUES (?, ?, 'concept', ?, ?, 'active', 0, ?, ?)`, page.id, page.slug, page.title, page.title+" summary", "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = database.Exec(`INSERT INTO compilation_runs (id, document_id, status, created_at) VALUES ('run_link', ?, 'validated', '2024-01-01T00:00:00Z')`, source.Document.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan := domain.CompilationPlan{DocumentID: source.Document.ID, SchemaVersion: "phase2-v1", PageActions: []domain.PageAction{{Action: domain.ActionLink, SourcePageID: "page_alpha", TargetPageID: "page_beta", Relation: "related_to", Reason: "connect related concepts"}}}
	if _, err := service.ApplyPlan(ctx, "run_link", plan); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM wiki_links WHERE source_page_id = 'page_alpha' AND target_page_id = 'page_beta' AND relation = 'related_to'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("wiki link count = %d, want 1", count)
	}
}

func TestLinkApplyResolvesPagesCreatedInSamePlan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceService := sources.Service{DB: database, DataRoot: root}
	source, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "graph.txt", MediaType: "text/plain", Data: []byte("evidence for two related new pages")})
	if err != nil {
		t.Fatal(err)
	}
	service := Service{DB: database, DataRoot: root}
	if _, err := database.Exec(`INSERT INTO compilation_runs (id, document_id, status, created_at) VALUES ('run_graph', ?, 'validated', '2024-01-01T00:00:00Z')`, source.Document.ID); err != nil {
		t.Fatal(err)
	}
	// The LINK is listed before the CREATE actions on purpose: apply must still
	// succeed because createsFirst reorders creations ahead of references.
	plan := domain.CompilationPlan{DocumentID: source.Document.ID, SchemaVersion: "phase2-v1", PageActions: []domain.PageAction{
		{Action: domain.ActionLink, SourcePageID: "zhou-ning", TargetPageID: "nuancheng-bakery", Relation: "part_of", Reason: "relate two new pages"},
		{Action: domain.ActionCreate, Slug: "zhou-ning", PageType: domain.PageTypeEntity, Title: "Zhou Ning", Summary: "A person", Reason: "new page", ClaimActions: []domain.ClaimAction{{Action: "ADD", Text: "works at the bakery", EvidenceChunkIDs: []string{source.Chunks[0].ID}}}},
		{Action: domain.ActionCreate, Slug: "nuancheng-bakery", PageType: domain.PageTypeEntity, Title: "Nuancheng Bakery", Summary: "A bakery", Reason: "new page", ClaimActions: []domain.ClaimAction{{Action: "ADD", Text: "is a bakery", EvidenceChunkIDs: []string{source.Chunks[0].ID}}}},
	}}
	if err := service.ValidatePlan(ctx, plan, source.Document.ID); err != nil {
		t.Fatalf("validate rejected a link to pages created in the same plan: %v", err)
	}
	if _, err := service.ApplyPlan(ctx, "run_graph", plan); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	var sourceID, targetID string
	if err := database.QueryRow(`SELECT id FROM wiki_pages WHERE slug = 'zhou-ning'`).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT id FROM wiki_pages WHERE slug = 'nuancheng-bakery'`).Scan(&targetID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM wiki_links WHERE source_page_id = ? AND target_page_id = ? AND relation = 'part_of'`, sourceID, targetID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("resolved wiki link count = %d, want 1", count)
	}
}

func TestRelateAllCreatesCrossPageLinks(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceService := sources.Service{DB: database, DataRoot: root}
	source, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "relate.txt", MediaType: "text/plain", Data: []byte("evidence for relate")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO compilation_runs (id, document_id, status, created_at) VALUES ('run_relate', ?, 'applied', '2024-01-01T00:00:00Z')`, source.Document.ID); err != nil {
		t.Fatal(err)
	}
	// Two pages from conceptually different documents: 周宁's summary names 暖橙面包店.
	for _, page := range []struct{ id, slug, title, summary string }{
		{"page_zhou", "zhou-ning", "周宁", "周宁是暖橙面包店的店主。"},
		{"page_bakery", "nuancheng-bakery", "暖橙面包店", "暖橙面包店位于青禾社区东门12号。"},
	} {
		if _, err = database.Exec(`INSERT INTO wiki_pages (id, slug, page_type, title, summary, status, current_revision, created_at, updated_at) VALUES (?, ?, 'entity', ?, ?, 'active', 0, ?, ?)`, page.id, page.slug, page.title, page.summary, "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	service := Service{DB: database, DataRoot: root, LLM: llm.DeterministicFake{}}
	added, err := service.RelateAll(ctx, "run_relate")
	if err != nil {
		t.Fatalf("RelateAll failed: %v", err)
	}
	if added < 1 {
		t.Fatalf("expected at least one cross-page link, got %d", added)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM wiki_links WHERE source_page_id = 'page_zhou' AND target_page_id = 'page_bakery' AND relation = 'references'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("cross-page link count = %d, want 1", count)
	}
	// Idempotent: a second pass adds nothing because the pair already links.
	again, err := service.RelateAll(ctx, "run_relate")
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("second RelateAll added %d links, want 0", again)
	}
}

func TestMergeApplyPreservesClaimsAndEvidence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceService := sources.Service{DB: database, DataRoot: root}
	source, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "merge.txt", MediaType: "text/plain", Data: []byte("evidence that must survive merge")})
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []struct{ id, slug, title string }{{"page_target", "vector-databases", "Vector Databases"}, {"page_source", "vector-database-systems", "Vector Database Systems"}} {
		_, err = database.Exec(`INSERT INTO wiki_pages (id, slug, page_type, title, summary, status, current_revision, created_at, updated_at) VALUES (?, ?, 'concept', ?, ?, 'active', 0, ?, ?)`, page.id, page.slug, page.title, page.title+" summary", "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = database.Exec(`INSERT INTO compilation_runs (id, document_id, status, created_at) VALUES ('run_merge', ?, 'validated', '2024-01-01T00:00:00Z')`, source.Document.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`INSERT INTO wiki_claims (id, page_id, text, claim_type, status, created_by_run_id, updated_by_run_id) VALUES ('claim_source', 'page_source', 'A claim from the duplicate page.', 'fact', 'active', 'run_merge', 'run_merge')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`INSERT INTO claim_evidence (claim_id, source_chunk_id, relation) VALUES ('claim_source', ?, 'supports')`, source.Chunks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{DB: database, DataRoot: root}
	plan := domain.CompilationPlan{DocumentID: source.Document.ID, SchemaVersion: "phase2-v1", PageActions: []domain.PageAction{{Action: domain.ActionMerge, SourcePageID: "page_source", TargetPageID: "page_target", Reason: "merge duplicate knowledge pages"}}}
	if _, err := service.ApplyPlan(ctx, "run_merge", plan); err != nil {
		t.Fatal(err)
	}
	var pageID, status string
	if err := database.QueryRow(`SELECT page_id FROM wiki_claims WHERE id = 'claim_source'`).Scan(&pageID); err != nil {
		t.Fatal(err)
	}
	if pageID != "page_target" {
		t.Fatalf("claim page = %s, want page_target", pageID)
	}
	if err := database.QueryRow(`SELECT status FROM wiki_pages WHERE id = 'page_source'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.PageStatusMerged) {
		t.Fatalf("source page status = %s", status)
	}
	var evidenceCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM claim_evidence WHERE claim_id = 'claim_source'`).Scan(&evidenceCount); err != nil {
		t.Fatal(err)
	}
	if evidenceCount != 1 {
		t.Fatalf("evidence count = %d, want 1", evidenceCount)
	}
	var linkCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM wiki_links WHERE source_page_id = 'page_source' AND target_page_id = 'page_target' AND relation = 'merged_into'`).Scan(&linkCount); err != nil {
		t.Fatal(err)
	}
	if linkCount != 1 {
		t.Fatalf("merged_into link count = %d, want 1", linkCount)
	}
}

func TestRenderFailureLeavesRetryableRun(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceService := sources.Service{DB: database, DataRoot: root}
	source, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "render.txt", MediaType: "text/plain", Data: []byte("renderable evidence")})
	if err != nil {
		t.Fatal(err)
	}
	renderCalls := 0
	service := Service{DB: database, DataRoot: root, LLM: llm.DeterministicFake{}, Render: func(string, []domain.WikiPage, []domain.WikiClaim, []domain.ClaimEvidence) error {
		renderCalls++
		if renderCalls == 1 {
			return errors.New("simulated renderer failure")
		}
		return nil
	}}
	result, err := service.Compile(ctx, source.Document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunRenderPending {
		t.Fatalf("compile status = %s, want %s", result.Status, RunRenderPending)
	}
	run, err := service.GetRun(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunRenderPending {
		t.Fatalf("stored run status = %s", run.Status)
	}
	var pages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM wiki_pages`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if pages != 1 {
		t.Fatalf("wiki pages after render failure = %d, want 1", pages)
	}
	if err := service.RetryRender(ctx, result.RunID); err != nil {
		t.Fatal(err)
	}
	run, err = service.GetRun(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunApplied {
		t.Fatalf("retried run status = %s, want %s", run.Status, RunApplied)
	}
}

func TestCompileUsesConfiguredOpenAICompatibleClient(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceService := sources.Service{DB: database, DataRoot: root}
	source, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "provider.md", MediaType: "text/markdown", Data: []byte("# Provider Concept\n\nKnowledge from a configured provider.")})
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	transport := compilerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Header.Get("Authorization") != "Bearer real-key" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		var payload struct {
			Model          string `json:"model"`
			ResponseFormat struct {
				Type       string `json:"type"`
				JSONSchema struct {
					Strict bool `json:"strict"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode provider request: %v", err)
		}
		if payload.Model != "real-model" || payload.ResponseFormat.Type != "json_schema" || !payload.ResponseFormat.JSONSchema.Strict {
			t.Errorf("provider payload = %#v", payload)
		}
		content := fmt.Sprintf(`{"document_id":%q,"summary":"Provider summary","topics":[{"key":"provider-concept","title":"Provider Concept","slug":"provider-concept","page_type":"concept","summary":"Provider summary","claims":[{"text":"Knowledge came from the configured provider.","claim_type":"fact","evidence_chunk_ids":[%q]}]}],"relations":[]}`, source.Document.ID, source.Chunks[0].ID)
		if calls == 2 {
			content = fmt.Sprintf(`{"document_id":%q,"page_actions":[{"action":"CREATE","slug":"provider-concept","page_type":"concept","title":"Provider Concept","summary":"Provider summary","reason":"compile provider knowledge","claim_actions":[{"action":"ADD","text":"Knowledge came from the configured provider.","claim_type":"fact","evidence_chunk_ids":[%q]}]}],"schema_version":"phase2-v1"}`, source.Document.ID, source.Chunks[0].ID)
		}
		body := `{"choices":[{"message":{"content":` + strconv.Quote(content) + `}}]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})
	client := llm.NewOpenAICompatible("https://provider.example/v1", "real-key", "real-model", "")
	client.HTTP = &http.Client{Transport: transport}
	service := Service{DB: database, DataRoot: root, LLM: client}
	result, err := service.Compile(ctx, source.Document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunApplied || calls != 2 {
		t.Fatalf("compile result/calls = %#v/%d", result, calls)
	}
	run, err := service.GetRun(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Model != "real-model" {
		t.Fatalf("run model = %q, want real-model", run.Model)
	}
}

func TestSemanticTitleVariantUpdatesExistingPage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceService := sources.Service{DB: database, DataRoot: root}
	service := Service{DB: database, DataRoot: root, LLM: llm.DeterministicFake{}}
	first, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "vector-a.md", MediaType: "text/markdown", Data: []byte("# Vector Databases\n\nVector databases store vector representations.")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Compile(ctx, first.Document.ID); err != nil {
		t.Fatal(err)
	}
	second, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "vector-b.md", MediaType: "text/markdown", Data: []byte("# Vector Database Systems\n\nVector database systems support similarity search.")})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Compile(ctx, second.Document.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.GetRun(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var plan domain.CompilationPlan
	if err := json.Unmarshal(run.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.PageActions) != 1 || plan.PageActions[0].Action != domain.ActionUpdate {
		t.Fatalf("variant title plan = %#v", plan)
	}
	var pages, sourcesCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM wiki_pages WHERE status = 'active'`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(DISTINCT sc.document_id) FROM wiki_claims wc JOIN claim_evidence ce ON ce.claim_id = wc.id JOIN source_chunks sc ON sc.id = ce.source_chunk_id WHERE wc.page_id = ?`, plan.PageActions[0].TargetPageID).Scan(&sourcesCount); err != nil {
		t.Fatal(err)
	}
	if pages != 1 || sourcesCount != 2 {
		t.Fatalf("active pages/sources = %d/%d, want 1/2", pages, sourcesCount)
	}
}

func TestChineseSemanticTitleVariantUpdatesExistingPage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceService := sources.Service{DB: database, DataRoot: root}
	service := Service{DB: database, DataRoot: root, LLM: llm.DeterministicFake{}}
	first, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "vector-zh-a.md", MediaType: "text/markdown", Data: []byte("# 向量数据库\n\n向量数据库用于存储和检索向量。")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Compile(ctx, first.Document.ID); err != nil {
		t.Fatal(err)
	}
	second, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "vector-zh-b.md", MediaType: "text/markdown", Data: []byte("# 向量数据库系统\n\n向量数据库系统支持相似度搜索。")})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Compile(ctx, second.Document.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.GetRun(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var plan domain.CompilationPlan
	if err := json.Unmarshal(run.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.PageActions) != 1 || plan.PageActions[0].Action != domain.ActionUpdate {
		t.Fatalf("Chinese variant plan = %#v", plan)
	}
	var pages, sourcesCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM wiki_pages WHERE status = 'active'`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(DISTINCT sc.document_id) FROM wiki_claims wc JOIN claim_evidence ce ON ce.claim_id = wc.id JOIN source_chunks sc ON sc.id = ce.source_chunk_id WHERE wc.page_id = ?`, plan.PageActions[0].TargetPageID).Scan(&sourcesCount); err != nil {
		t.Fatal(err)
	}
	if pages != 1 || sourcesCount != 2 {
		t.Fatalf("Chinese active pages/sources = %d/%d, want 1/2", pages, sourcesCount)
	}
}

func TestLexicalSimilaritySupportsChineseTitleVariants(t *testing.T) {
	score := lexicalSimilarity("向量数据库系统", "向量数据库")
	if score < 0.20 {
		t.Fatalf("Chinese title similarity = %0.3f, want at least candidate threshold 0.20", score)
	}
}

func TestUnsafeCreateSlugFailsBeforeApplyAndRender(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceService := sources.Service{DB: database, DataRoot: root}
	source, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "unsafe.md", MediaType: "text/markdown", Data: []byte("# Unsafe\n\nEvidence that must not be applied.")})
	if err != nil {
		t.Fatal(err)
	}
	service := Service{DB: database, DataRoot: root, LLM: unsafeSlugClient{ChunkID: source.Chunks[0].ID}}
	result, err := service.Compile(ctx, source.Document.ID)
	if err == nil || result.Status != RunFailed {
		t.Fatalf("unsafe slug compile result/error = %#v/%v", result, err)
	}
	if !strings.Contains(err.Error(), "invalid slug") {
		t.Fatalf("unsafe slug error = %v", err)
	}
	var pages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM wiki_pages`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if pages != 0 {
		t.Fatalf("wiki pages after rejected slug = %d, want 0", pages)
	}
	if _, err := os.Stat(filepath.Join(root, "wiki")); !os.IsNotExist(err) {
		t.Fatalf("wiki directory after rejected slug: stat error = %v", err)
	}
}

func TestRendererRejectsUnsafeSlug(t *testing.T) {
	root := t.TempDir()
	err := RenderMarkdown(root, []domain.WikiPage{{ID: "page_1", Slug: "../escape", PageType: domain.PageTypeConcept, Title: "Unsafe", Summary: "Unsafe", Status: domain.PageStatusActive}}, nil, nil)
	if err == nil {
		t.Fatal("renderer accepted unsafe slug")
	}
	if _, statErr := os.Stat(filepath.Join(root, "wiki")); !os.IsNotExist(statErr) {
		t.Fatalf("wiki directory after renderer rejection: stat error = %v", statErr)
	}
}

type unsafeSlugClient struct {
	ChunkID string
}

func (c unsafeSlugClient) Analyze(_ context.Context, input llm.AnalyzeInput) (json.RawMessage, error) {
	return json.Marshal(domain.SourceAnalysis{
		DocumentID: input.Document.ID,
		Summary:    "Unsafe slug fixture",
		Topics:     []domain.AnalyzedTopic{{Key: "unsafe", Title: "Unsafe", Slug: "unsafe", PageType: domain.PageTypeConcept, Summary: "Unsafe", Claims: []domain.AnalyzedClaim{{Text: "Evidence", ClaimType: domain.ClaimFact, EvidenceChunkIDs: []string{c.ChunkID}}}}},
	})
}

func (c unsafeSlugClient) Plan(_ context.Context, input llm.PlanInput) (json.RawMessage, error) {
	return json.Marshal(domain.CompilationPlan{DocumentID: input.DocumentID, SchemaVersion: "phase2-v1", PageActions: []domain.PageAction{{Action: domain.ActionCreate, Slug: "../escape", PageType: domain.PageTypeConcept, Title: "Unsafe", Summary: "Unsafe", Reason: "malicious fixture", ClaimActions: []domain.ClaimAction{{Action: "ADD", Text: "Evidence", ClaimType: domain.ClaimFact, EvidenceChunkIDs: []string{c.ChunkID}}}}}})
}

func (c unsafeSlugClient) SuggestLinks(_ context.Context, _ llm.LinkInput) (json.RawMessage, error) {
	return json.Marshal(domain.LinkSuggestions{Links: []domain.LinkSuggestion{}})
}

type compilerRoundTripFunc func(*http.Request) (*http.Response, error)

func (f compilerRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDeleteSourceRemovesExclusivePagesAndKeepsShared(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceService := sources.Service{DB: database, DataRoot: root}
	sourceA, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "a.txt", MediaType: "text/plain", Data: []byte("alpha source evidence")})
	if err != nil {
		t.Fatal(err)
	}
	sourceB, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "b.txt", MediaType: "text/plain", Data: []byte("beta source evidence")})
	if err != nil {
		t.Fatal(err)
	}
	chunkA, chunkB := sourceA.Chunks[0].ID, sourceB.Chunks[0].ID
	exec := func(query string, args ...any) {
		if _, err := database.Exec(query, args...); err != nil {
			t.Fatalf("setup exec failed: %v\n%s", err, query)
		}
	}
	exec(`INSERT INTO compilation_runs (id, document_id, status, created_at) VALUES ('run_a', ?, 'applied', '2024-01-01T00:00:00Z')`, sourceA.Document.ID)
	exec(`INSERT INTO compilation_runs (id, document_id, status, created_at) VALUES ('run_b', ?, 'applied', '2024-01-02T00:00:00Z')`, sourceB.Document.ID)
	for _, p := range []struct{ id, slug, title string }{{"page_excl_a", "excl-a", "Excl A"}, {"page_shared", "shared", "Shared"}, {"page_excl_b", "excl-b", "Excl B"}} {
		exec(`INSERT INTO wiki_pages (id, slug, page_type, title, summary, status, current_revision, created_at, updated_at) VALUES (?, ?, 'concept', ?, ?, 'active', 0, ?, ?)`, p.id, p.slug, p.title, p.title+" summary", "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z")
	}
	// claims: page_excl_a<-A; page_shared<-A and <-B; page_excl_b<-B
	for _, c := range []struct{ id, page, run, chunk string }{{"c_a1", "page_excl_a", "run_a", chunkA}, {"c_a2", "page_shared", "run_a", chunkA}, {"c_b1", "page_shared", "run_b", chunkB}, {"c_b2", "page_excl_b", "run_b", chunkB}} {
		exec(`INSERT INTO wiki_claims (id, page_id, text, claim_type, status, created_by_run_id, updated_by_run_id) VALUES (?, ?, ?, 'fact', 'active', ?, ?)`, c.id, c.page, c.id+" text", c.run, c.run)
		exec(`INSERT INTO claim_evidence (claim_id, source_chunk_id, relation) VALUES (?, ?, 'supports')`, c.id, c.chunk)
	}
	// links: excl_a->shared (run_a), excl_b->shared (run_b)
	exec(`INSERT INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id) VALUES ('page_excl_a', 'page_shared', 'related_to', 'run_a')`)
	exec(`INSERT INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id) VALUES ('page_excl_b', 'page_shared', 'related_to', 'run_b')`)

	service := Service{DB: database, DataRoot: root, LLM: llm.DeterministicFake{}}
	// SENTINEL_DELETE_SOURCE_TEST
	summary, err := service.DeleteSource(ctx, sourceA.Document.ID)
	if err != nil {
		t.Fatalf("DeleteSource failed: %v", err)
	}
	if summary.DeletedPages != 1 || summary.DeletedClaims != 2 {
		t.Fatalf("summary = %+v, want 1 page / 2 claims (exclusive page's claim + shared page's A-only claim)", summary)
	}
	exists := func(query string, args ...any) int {
		var n int
		if err := database.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := exists(`SELECT COUNT(*) FROM wiki_pages WHERE id = 'page_excl_a'`); n != 0 {
		t.Fatalf("exclusive page not deleted: %d", n)
	}
	if n := exists(`SELECT COUNT(*) FROM wiki_pages WHERE id IN ('page_shared','page_excl_b')`); n != 2 {
		t.Fatalf("shared/other pages should survive: %d", n)
	}
	if n := exists(`SELECT COUNT(*) FROM wiki_claims WHERE id = 'c_a2'`); n != 0 {
		t.Fatalf("source A claim on shared page not removed: %d", n)
	}
	if n := exists(`SELECT COUNT(*) FROM wiki_claims WHERE id = 'c_b1'`); n != 1 {
		t.Fatalf("source B claim on shared page must remain: %d", n)
	}
	if n := exists(`SELECT COUNT(*) FROM wiki_links WHERE source_page_id = 'page_excl_b' AND target_page_id = 'page_shared'`); n != 1 {
		t.Fatalf("surviving link removed: %d", n)
	}
	if n := exists(`SELECT COUNT(*) FROM source_documents WHERE id = ?`, sourceA.Document.ID); n != 0 {
		t.Fatalf("source A row not deleted: %d", n)
	}
	if n := exists(`SELECT COUNT(*) FROM source_chunks WHERE document_id = ?`, sourceA.Document.ID); n != 0 {
		t.Fatalf("source A chunks not deleted: %d", n)
	}
	if n := exists(`SELECT COUNT(*) FROM compilation_runs WHERE id = 'run_a'`); n != 0 {
		t.Fatalf("run_a not deleted: %d", n)
	}
	if _, err := os.Stat(sourceA.Document.OriginalPath); !os.IsNotExist(err) {
		t.Fatalf("original file for A still present: %v", err)
	}
	if _, err := os.Stat(sourceB.Document.OriginalPath); err != nil {
		t.Fatalf("original file for B should remain: %v", err)
	}
}

func TestDeletePageRemovesPageClaimsAndLinks(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceService := sources.Service{DB: database, DataRoot: root}
	source, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "page.txt", MediaType: "text/plain", Data: []byte("page delete evidence")})
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		if _, err := database.Exec(query, args...); err != nil {
			t.Fatalf("setup exec failed: %v", err)
		}
	}
	exec(`INSERT INTO compilation_runs (id, document_id, status, created_at) VALUES ('run_p', ?, 'applied', '2024-01-01T00:00:00Z')`, source.Document.ID)
	for _, p := range []struct{ id, slug string }{{"page_x", "page-x"}, {"page_y", "page-y"}} {
		exec(`INSERT INTO wiki_pages (id, slug, page_type, title, summary, status, current_revision, created_at, updated_at) VALUES (?, ?, 'concept', ?, ?, 'active', 0, ?, ?)`, p.id, p.slug, p.id, p.id+" summary", "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z")
	}
	exec(`INSERT INTO wiki_claims (id, page_id, text, claim_type, status, created_by_run_id, updated_by_run_id) VALUES ('cx', 'page_x', 'x', 'fact', 'active', 'run_p', 'run_p')`)
	exec(`INSERT INTO claim_evidence (claim_id, source_chunk_id, relation) VALUES ('cx', ?, 'supports')`, source.Chunks[0].ID)
	exec(`INSERT INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id) VALUES ('page_y', 'page_x', 'related_to', 'run_p')`)

	service := Service{DB: database, DataRoot: root}
	if err := service.DeletePage(ctx, "page-x"); err != nil {
		t.Fatalf("DeletePage failed: %v", err)
	}
	count := func(query string) int {
		var n int
		if err := database.QueryRow(query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(`SELECT COUNT(*) FROM wiki_pages WHERE id = 'page_x'`) != 0 {
		t.Fatal("page_x not deleted")
	}
	if count(`SELECT COUNT(*) FROM wiki_pages WHERE id = 'page_y'`) != 1 {
		t.Fatal("page_y should remain")
	}
	if count(`SELECT COUNT(*) FROM wiki_claims WHERE id = 'cx'`) != 0 {
		t.Fatal("claim cx not deleted")
	}
	if count(`SELECT COUNT(*) FROM wiki_links WHERE source_page_id = 'page_y' AND target_page_id = 'page_x'`) != 0 {
		t.Fatal("dangling link to page_x not removed")
	}
	if count(`SELECT COUNT(*) FROM source_documents`) != 1 {
		t.Fatal("source should be untouched by page delete")
	}
	if err := service.DeletePage(ctx, "missing-slug"); err == nil {
		t.Fatal("expected error for missing page")
	}
}


func TestPluralTypeUsesEntities(t *testing.T) {
	cases := map[domain.PageType]string{domain.PageTypeConcept: "concepts", domain.PageTypeEntity: "entities", domain.PageTypeTopic: "topics"}
	for pt, want := range cases {
		if got := pluralType(pt); got != want {
			t.Fatalf("pluralType(%q) = %q, want %q", pt, got, want)
		}
	}
}

func TestLinkifyMarkdownEmbedsInlineLinks(t *testing.T) {
	conns := []pageRef{{slug: "nuancheng-bakery", title: "暖橙面包店"}, {slug: "doubao", title: "豆包"}, {slug: "doubao-care", title: "豆包照护"}}
	got := linkifyMarkdown("黄油可颂是暖橙面包店的招牌商品。", conns)
	if want := "黄油可颂是[[nuancheng-bakery|暖橙面包店]]的招牌商品。"; got != want {
		t.Fatalf("inline link got %q want %q", got, want)
	}
	// Longest title wins: "豆包照护" preferred over "豆包".
	if got := linkifyMarkdown("确认豆包照护安排", conns); got != "确认[[doubao-care|豆包照护]]安排" {
		t.Fatalf("longest-match got %q", got)
	}
	if linkifyMarkdown("暖橙面包店", nil) != "暖橙面包店" {
		t.Fatal("nil connections must leave text unchanged")
	}
}
