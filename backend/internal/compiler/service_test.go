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

type compilerRoundTripFunc func(*http.Request) (*http.Response, error)

func (f compilerRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
