package wiki

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeychen/llm-wiki-demo/backend/internal/compiler"
	appdb "github.com/joeychen/llm-wiki-demo/backend/internal/db"
	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	"github.com/joeychen/llm-wiki-demo/backend/internal/sources"
)

func TestWikiServiceBrowsesProvenanceLinksRevisionsAndLint(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	sourceService := sources.Service{DB: database, DataRoot: root}
	compilerService := compiler.Service{DB: database, DataRoot: root, LLM: llm.DeterministicFake{}}
	first, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "wiki-a.md", MediaType: "text/markdown", Data: []byte("# Shared Topic\n\nSource A establishes the baseline.")})
	if err != nil {
		t.Fatal(err)
	}
	firstRun, err := compilerService.Compile(ctx, first.Document.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "wiki-b.md", MediaType: "text/markdown", Data: []byte("# Shared Topic\n\nSource B adds corroborating evidence.")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compilerService.Compile(ctx, second.Document.ID); err != nil {
		t.Fatal(err)
	}

	var pageID string
	if err := database.QueryRow(`SELECT id FROM wiki_pages WHERE slug = 'shared-topic'`).Scan(&pageID); err != nil {
		t.Fatal(err)
	}
	service := Service{DB: database}
	pages, err := service.ListPages(ctx, "topic")
	if err != nil || len(pages) != 1 || pages[0].ClaimCount != 2 || pages[0].SourceCount != 2 {
		t.Fatalf("page list = %#v, err = %v", pages, err)
	}
	detail, err := service.GetPage(ctx, pageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Claims) != 2 || len(detail.Sections) != 1 || len(detail.Sources) != 2 {
		t.Fatalf("page detail claims/sections/sources = %d/%d/%d", len(detail.Claims), len(detail.Sections), len(detail.Sources))
	}
	for _, claim := range detail.Claims {
		if len(claim.Evidence) != 1 || claim.Evidence[0].Chunk.ID == "" || claim.Evidence[0].Source.ID == "" {
			t.Fatalf("claim provenance = %#v", claim)
		}
	}
	trace, err := service.SourceTrace(ctx, second.Document.ID)
	if err != nil || trace.ChunkCount != 1 || trace.ClaimCount != 1 || len(trace.EvidenceHits) != 1 {
		t.Fatalf("source trace = %#v, err = %v", trace, err)
	}
	if trace.EvidenceHits[0].Chunk.DocumentID != second.Document.ID {
		t.Fatalf("trace chunk document = %q", trace.EvidenceHits[0].Chunk.DocumentID)
	}

	_, err = database.Exec(`INSERT INTO wiki_pages (id, slug, page_type, title, summary, status, current_revision, created_at, updated_at) VALUES ('page-related', 'related-page', 'concept', 'Related Page', 'Related', 'active', 0, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.Exec(`INSERT INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id) VALUES (?, 'page-related', 'related_to', ?)`, pageID, firstRun.RunID); err != nil {
		t.Fatal(err)
	}
	related, err := service.GetPage(ctx, pageID)
	if err != nil || len(related.Links) != 1 || related.Links[0].Page.ID != "page-related" {
		t.Fatalf("outgoing links = %#v, err = %v", related.Links, err)
	}
	backlink, err := service.GetPage(ctx, "page-related")
	if err != nil || len(backlink.Backlinks) != 1 || backlink.Backlinks[0].Page.ID != pageID {
		t.Fatalf("backlinks = %#v, err = %v", backlink.Backlinks, err)
	}
	revisions, err := service.Revisions(ctx, pageID)
	if err != nil || len(revisions) != 2 {
		t.Fatalf("revisions = %#v, err = %v", revisions, err)
	}
	diff, err := service.Diff(ctx, pageID, 2)
	if err != nil || len(diff.AddedClaims) != 1 {
		t.Fatalf("revision diff = %#v, err = %v", diff, err)
	}

	if _, err = database.Exec(`INSERT INTO wiki_claims (id, page_id, text, claim_type, status, created_by_run_id, updated_by_run_id) VALUES ('claim-no-evidence', ?, 'Broken claim', 'fact', 'active', ?, ?)`, pageID, firstRun.RunID, firstRun.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Exec(`INSERT INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id) VALUES ('missing-source', ?, 'related_to', ?)`, pageID, firstRun.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	issues, err := service.Lint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	issueCodes := make([]string, 0, len(issues))
	for _, issue := range issues {
		issueCodes = append(issueCodes, issue.Code)
	}
	joined := strings.Join(issueCodes, ",")
	if !strings.Contains(joined, "broken_link") || !strings.Contains(joined, "claim_without_evidence") {
		t.Fatalf("lint issues = %#v", issues)
	}
}

func TestMarkdownProjectionIncludesDeterministicIndexAndTypedPage(t *testing.T) {
	root := t.TempDir()
	page := domain.WikiPage{ID: "page-1", Slug: "compiled-topic", PageType: domain.PageTypeTopic, Title: "Compiled Topic", Summary: "Summary", Status: domain.PageStatusActive, CurrentRevision: 1}
	related := domain.WikiPage{ID: "page-2", Slug: "related-topic", PageType: domain.PageTypeConcept, Title: "Related Topic", Summary: "Related summary", Status: domain.PageStatusActive, CurrentRevision: 1}
	links := []domain.WikiLink{{SourcePageID: page.ID, TargetPageID: related.ID, Relation: "related_to", CreatedByRunID: "run-1"}}
	if err := compiler.RenderMarkdownWithLinks(root, []domain.WikiPage{page, related}, nil, nil, links); err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(filepath.Join(root, "wiki", "index.md"))
	if err != nil || !strings.Contains(string(index), "topics/compiled-topic.md") {
		t.Fatalf("index = %s, err = %v", index, err)
	}
	pageBytes, err := os.ReadFile(filepath.Join(root, "wiki", "topics", "compiled-topic.md"))
	if err != nil || !strings.Contains(string(pageBytes), "# Compiled Topic") || !strings.Contains(string(pageBytes), "[[related-topic]] (`related_to`)") {
		t.Fatalf("typed page = %s, err = %v", pageBytes, err)
	}
	first := string(index) + string(pageBytes)
	if err := compiler.RenderMarkdownWithLinks(root, []domain.WikiPage{page, related}, nil, nil, links); err != nil {
		t.Fatal(err)
	}
	index, err = os.ReadFile(filepath.Join(root, "wiki", "index.md"))
	pageBytes, readErr := os.ReadFile(filepath.Join(root, "wiki", "topics", "compiled-topic.md"))
	if err != nil || readErr != nil || first != string(index)+string(pageBytes) {
		t.Fatal("repeated Markdown render was not deterministic")
	}
}

func TestMergeHidesDeprecatedPageAndKeepsCanonicalKnowledge(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := appdb.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	sourceService := sources.Service{DB: database, DataRoot: root}
	source, err := sourceService.Ingest(ctx, sources.IngestInput{OriginalName: "merge.md", MediaType: "text/markdown", Data: []byte("# Duplicate Topic\n\nEvidence that must survive the merge.")})
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []domain.WikiPage{
		{ID: "page-a", Slug: "canonical-topic", PageType: domain.PageTypeConcept, Title: "Canonical Topic", Summary: "Canonical", Status: domain.PageStatusActive},
		{ID: "page-b", Slug: "duplicate-topic", PageType: domain.PageTypeConcept, Title: "Duplicate Topic", Summary: "Duplicate", Status: domain.PageStatusActive},
	} {
		if _, err := database.Exec(`INSERT INTO wiki_pages (id, slug, page_type, title, summary, status, current_revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 0, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`, page.ID, page.Slug, page.PageType, page.Title, page.Summary, page.Status); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(`INSERT INTO compilation_runs (id, document_id, status, created_at) VALUES ('run-merge-view', ?, 'validated', '2024-01-01T00:00:00Z')`, source.Document.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO wiki_claims (id, page_id, text, claim_type, status, created_by_run_id, updated_by_run_id) VALUES ('claim-b', 'page-b', 'Knowledge retained after merge.', 'fact', 'active', 'run-merge-view', 'run-merge-view')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO claim_evidence (claim_id, source_chunk_id, relation) VALUES ('claim-b', ?, 'supports')`, source.Chunks[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id) VALUES ('page-a', 'page-b', 'related_to', 'run-merge-view')`); err != nil {
		t.Fatal(err)
	}
	if err := compiler.RenderMarkdownWithLinks(root,
		[]domain.WikiPage{
			{ID: "page-a", Slug: "canonical-topic", PageType: domain.PageTypeConcept, Title: "Canonical Topic", Summary: "Canonical", Status: domain.PageStatusActive},
			{ID: "page-b", Slug: "duplicate-topic", PageType: domain.PageTypeConcept, Title: "Duplicate Topic", Summary: "Duplicate", Status: domain.PageStatusActive},
		}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(root, "wiki", "concepts", "duplicate-topic.md")
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("pre-merge page file: %v", err)
	}

	compilerService := compiler.Service{DB: database, DataRoot: root}
	plan := domain.CompilationPlan{DocumentID: source.Document.ID, SchemaVersion: "phase2-v1", PageActions: []domain.PageAction{{Action: domain.ActionMerge, SourcePageID: "page-b", TargetPageID: "page-a", Reason: "merge duplicate page"}}}
	if _, err := compilerService.ApplyPlan(ctx, "run-merge-view", plan); err != nil {
		t.Fatal(err)
	}

	service := Service{DB: database, DataRoot: root}
	pages, err := service.ListPages(ctx, "")
	if err != nil || len(pages) != 1 || pages[0].ID != "page-a" {
		t.Fatalf("active pages = %#v, err = %v", pages, err)
	}
	canonical, err := service.GetPage(ctx, "page-a")
	if err != nil || len(canonical.Backlinks) != 0 || len(canonical.Links) != 0 || len(canonical.Claims) != 1 {
		t.Fatalf("canonical detail = %#v, err = %v", canonical, err)
	}
	merged, err := service.GetPage(ctx, "duplicate-topic")
	if err != nil || merged.Page.Status != domain.PageStatusMerged || merged.CanonicalPage == nil || merged.CanonicalPage.ID != "page-a" || len(merged.Claims) != 0 {
		t.Fatalf("merged stub = %#v, err = %v", merged, err)
	}
	index, err := os.ReadFile(filepath.Join(root, "wiki", "index.md"))
	if err != nil || strings.Contains(string(index), "duplicate-topic") || !strings.Contains(string(index), "canonical-topic") {
		t.Fatalf("post-merge index = %s, err = %v", index, err)
	}
	for _, path := range []string{oldPath, filepath.Join(root, "wiki", "duplicate-topic.md")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("merged page projection remains at %s: %v", path, err)
		}
	}
	var claimPage string
	var evidenceCount int
	if err := database.QueryRow(`SELECT page_id FROM wiki_claims WHERE id = 'claim-b'`).Scan(&claimPage); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM claim_evidence WHERE claim_id = 'claim-b'`).Scan(&evidenceCount); err != nil {
		t.Fatal(err)
	}
	if claimPage != "page-a" || evidenceCount != 1 {
		t.Fatalf("claim/evidence after merge = %s/%d", claimPage, evidenceCount)
	}
	issues, err := service.Lint(ctx)
	if err != nil || len(issues) != 0 {
		t.Fatalf("lint after valid merge = %#v, err = %v", issues, err)
	}

	if _, err := database.Exec(`INSERT INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id) VALUES ('page-a', 'page-b', 'related_to', 'run-merge-view')`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("stale merged page"), 0o644); err != nil {
		t.Fatal(err)
	}
	issues, err = service.Lint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	codes := make([]string, 0, len(issues))
	for _, issue := range issues {
		codes = append(codes, issue.Code)
	}
	joined := strings.Join(codes, ",")
	if !strings.Contains(joined, "inactive_page_link") || !strings.Contains(joined, "inactive_page_projection") {
		t.Fatalf("lint did not detect merged exposure: %#v", issues)
	}
}
