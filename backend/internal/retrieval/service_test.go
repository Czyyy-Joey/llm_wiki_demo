package retrieval

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/joeychen/llm-wiki-demo/backend/internal/db"
	"github.com/joeychen/llm-wiki-demo/backend/internal/indexing"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
)

type semanticTestEmbedder struct{}

func (semanticTestEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	result := make([][]float32, len(inputs))
	for i, input := range inputs {
		value := strings.ToLower(input)
		if strings.Contains(value, "vector database") || strings.Contains(value, "nearest neighbor lookup") {
			result[i] = []float32{1, 0}
		} else {
			result[i] = []float32{0, 1}
		}
	}
	return result, nil
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(context.Background(), "file:"+filepath.Join(t.TempDir(), "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func insertPage(t *testing.T, database *sql.DB, id, slug, title, status string) {
	t.Helper()
	_, err := database.Exec(`INSERT INTO wiki_pages (id, slug, page_type, title, summary, status, current_revision, created_at, updated_at) VALUES (?, ?, 'concept', ?, ?, ?, 1, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`, id, slug, title, title+" summary", status)
	if err != nil {
		t.Fatal(err)
	}
}

func TestHybridRetrievalReindexExpansionAndRebuild(t *testing.T) {
	database := testDB(t)
	defer database.Close()
	insertPage(t, database, "page_vector", "vector-databases", "Vector Databases", "active")
	insertPage(t, database, "page_index", "indexing", "Indexing", "active")
	insertPage(t, database, "page_old", "old-page", "Old Page", "merged")
	_, err := database.Exec(`INSERT INTO source_documents (id, original_name, media_type, sha256, original_path, status, parser_version, created_at) VALUES ('doc_1', 'notes.txt', 'text/plain', 'hash', 'original', 'parsed', 'v1', '2024-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`INSERT INTO source_chunks (id, document_id, chunk_index, text, char_start, char_end, content_hash) VALUES ('chunk_1', 'doc_1', 0, 'Vector databases store embeddings for semantic search.', 0, 54, 'chunk-hash')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`INSERT INTO compilation_runs (id, document_id, status, created_at) VALUES ('run_1', 'doc_1', 'applied', '2024-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`INSERT INTO wiki_claims (id, page_id, text, claim_type, status, created_by_run_id, updated_by_run_id) VALUES ('claim_1', 'page_vector', 'Vector databases store embeddings for semantic search.', 'fact', 'active', 'run_1', 'run_1'), ('claim_2', 'page_index', 'Indexes make retrieval faster.', 'fact', 'active', 'run_1', 'run_1')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`INSERT INTO claim_evidence (claim_id, source_chunk_id, relation) VALUES ('claim_1', 'chunk_1', 'supports')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`INSERT INTO wiki_links (source_page_id, target_page_id, relation, created_by_run_id) VALUES ('page_vector', 'page_index', 'related_to', 'run_1'), ('page_old', 'page_vector', 'merged_into', 'run_1')`)
	if err != nil {
		t.Fatal(err)
	}

	indexDir := filepath.Join(t.TempDir(), "indexes")
	indexer := indexing.Service{DB: database, Embedder: llm.DeterministicEmbedding{}, BatchSize: 2, IndexDir: indexDir}
	first, err := indexer.Reindex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.WikiPassages != 2 || first.SourceChunks != 1 {
		t.Fatalf("reindex result = %#v", first)
	}
	var mergedCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM wiki_passages WHERE page_id = 'page_old'`).Scan(&mergedCount); err != nil {
		t.Fatal(err)
	}
	if mergedCount != 0 {
		t.Fatalf("merged page indexed: %d", mergedCount)
	}

	// Corrupt the SQLite vector cache after indexing. Vector recall still comes
	// from chromem-go while SQLite remains the canonical Wiki/evidence store.
	if _, err := database.Exec(`UPDATE wiki_passages SET embedding_json = '[]'; UPDATE source_chunk_embeddings SET embedding_json = '[]'`); err != nil {
		t.Fatal(err)
	}
	retriever := Service{DB: database, Embedder: llm.DeterministicEmbedding{}, TopK: 1, ContextBudget: 1000, IndexDir: indexDir}
	result, err := retriever.Search(context.Background(), "semantic embeddings")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Trace.FTSCandidates) == 0 || len(result.Trace.VectorCandidates) == 0 {
		t.Fatalf("hybrid trace = %#v", result.Trace)
	}
	if len(result.Candidates) == 0 || result.Candidates[0].PageID != "page_vector" {
		t.Fatalf("candidates = %#v", result.Candidates)
	}
	if len(result.Trace.ExpandedPages) == 0 {
		t.Fatalf("expected link expansion: %#v", result.Trace)
	}
	if len(result.Trace.Candidates) < 2 || len(result.Trace.Candidates[1].ExpansionFrom) == 0 {
		t.Fatalf("trace lacks expansion source: %#v", result.Trace.Candidates)
	}
	if len(result.Trace.Candidates) == 0 || result.Trace.Candidates[0].FinalScore == 0 {
		t.Fatalf("trace lacks computed candidate scores: %#v", result.Trace.Candidates)
	}
	if result.Trace.ContextBudget != 1000 || result.Trace.ContextUsed == 0 || result.Trace.ContextUsed > result.Trace.ContextBudget {
		t.Fatalf("context budget trace = %#v", result.Trace)
	}
	if len(result.Context) == 0 || result.Context[0].Kind != "wiki" {
		t.Fatalf("context = %#v", result.Context)
	}
	if err := result.Trace.Validate(); err != nil {
		t.Fatal(err)
	}

	if _, err := database.Exec(`DELETE FROM wiki_fts; DELETE FROM wiki_passages; DELETE FROM source_chunk_embeddings;`); err != nil {
		t.Fatal(err)
	}
	second, err := indexer.Reindex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.WikiPassages != first.WikiPassages || second.SourceChunks != first.SourceChunks {
		t.Fatalf("rebuilt result = %#v", second)
	}
}

func TestSemanticRewriteUsesWikiVectorRecall(t *testing.T) {
	database := testDB(t)
	defer database.Close()
	insertPage(t, database, "page_vector", "vector-databases", "Vector Databases", "active")
	insertPage(t, database, "page_other", "relational-model", "Relational Model", "active")
	indexDir := filepath.Join(t.TempDir(), "indexes")
	indexer := indexing.Service{DB: database, Embedder: semanticTestEmbedder{}, Model: "semantic-test-v1", IndexDir: indexDir}
	if _, err := indexer.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := (Service{DB: database, Embedder: semanticTestEmbedder{}, TopK: 1, IndexDir: indexDir}).Search(context.Background(), "nearest neighbor lookup")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Trace.FTSCandidates) != 0 {
		t.Fatalf("semantic rewrite unexpectedly matched FTS: %#v", result.Trace.FTSCandidates)
	}
	if len(result.Trace.VectorCandidates) == 0 || result.Trace.VectorCandidates[0] != "page_vector" {
		t.Fatalf("vector candidates = %#v", result.Trace.VectorCandidates)
	}
	if len(result.Candidates) == 0 || result.Candidates[0].PageID != "page_vector" {
		t.Fatalf("semantic candidates = %#v", result.Candidates)
	}
}

func TestEqualVectorSimilarityHasStableOrderingAcrossStoresAndReindex(t *testing.T) {
	database := testDB(t)
	defer database.Close()
	for _, page := range []struct{ id, slug, title string }{
		{"page_h", "hotel", "Hotel"},
		{"page_a", "alpha", "Alpha"},
		{"page_f", "foxtrot", "Foxtrot"},
		{"page_c", "charlie", "Charlie"},
		{"page_g", "golf", "Golf"},
		{"page_b", "bravo", "Bravo"},
		{"page_e", "echo", "Echo"},
		{"page_d", "delta", "Delta"},
	} {
		insertPage(t, database, page.id, page.slug, page.title, "active")
	}

	indexDir := filepath.Join(t.TempDir(), "indexes")
	indexer := indexing.Service{DB: database, Embedder: semanticTestEmbedder{}, Model: "equal-vector-v1", IndexDir: indexDir}
	if _, err := indexer.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}

	expected := expectedVectorPageOrder(t, database)[:6]
	chromemService := Service{DB: database, Embedder: semanticTestEmbedder{}, TopK: 3, ContextBudget: 1000, IndexDir: indexDir}
	first := retrievalOrdering(t, chromemService, "query without matching terms")
	if !reflect.DeepEqual(first.vector, expected) {
		t.Fatalf("chromem vector order = %v, want %v", first.vector, expected)
	}
	for i := 0; i < 5; i++ {
		if got := retrievalOrdering(t, chromemService, "query without matching terms"); !reflect.DeepEqual(got, first) {
			t.Fatalf("chromem query %d ordering = %#v, want %#v", i+2, got, first)
		}
	}

	sqliteService := chromemService
	sqliteService.IndexDir = filepath.Join(t.TempDir(), "missing-indexes")
	if got := retrievalOrdering(t, sqliteService, "query without matching terms"); !reflect.DeepEqual(got, first) {
		t.Fatalf("SQLite fallback ordering = %#v, want chromem %#v", got, first)
	}

	if _, err := indexer.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := retrievalOrdering(t, chromemService, "query without matching terms"); !reflect.DeepEqual(got, first) {
		t.Fatalf("post-reindex ordering = %#v, want %#v", got, first)
	}
}

type observedOrdering struct {
	vector     []string
	candidates []string
	context    []string
}

func retrievalOrdering(t *testing.T, service Service, query string) observedOrdering {
	t.Helper()
	result, err := service.Search(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	observed := observedOrdering{vector: append([]string(nil), result.Trace.VectorCandidates...)}
	for _, candidate := range result.Candidates {
		observed.candidates = append(observed.candidates, candidate.PageID)
	}
	for _, item := range result.Context {
		observed.context = append(observed.context, item.ID)
	}
	return observed
}

func expectedVectorPageOrder(t *testing.T, database *sql.DB) []string {
	t.Helper()
	rows, err := database.Query(`SELECT id, page_id FROM wiki_passages`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type passage struct{ id, pageID string }
	passages := []passage{}
	for rows.Next() {
		var item passage
		if err := rows.Scan(&item.id, &item.pageID); err != nil {
			t.Fatal(err)
		}
		passages = append(passages, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(passages, func(i, j int) bool { return passages[i].id < passages[j].id })
	result := make([]string, len(passages))
	for i, item := range passages {
		result[i] = item.pageID
	}
	return result
}

func TestStaleChromemEntryForMergedPageIsNotRecalled(t *testing.T) {
	database := testDB(t)
	defer database.Close()
	insertPage(t, database, "page_active", "active-page", "Active Page", "active")
	insertPage(t, database, "page_later_merged", "retired-page", "Retired Page", "active")
	indexDir := filepath.Join(t.TempDir(), "indexes")
	indexer := indexing.Service{DB: database, Embedder: llm.DeterministicEmbedding{}, IndexDir: indexDir}
	if _, err := indexer.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE wiki_pages SET status = 'merged' WHERE id = 'page_later_merged'`); err != nil {
		t.Fatal(err)
	}
	result, err := (Service{DB: database, Embedder: llm.DeterministicEmbedding{}, IndexDir: indexDir}).Search(context.Background(), "Retired Page")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range result.Candidates {
		if candidate.PageID == "page_later_merged" {
			t.Fatalf("merged page recalled from stale chromem index: %#v", candidate)
		}
	}
}

func TestChineseWikiMatchingTokensReachFTS(t *testing.T) {
	database := testDB(t)
	defer database.Close()
	insertPage(t, database, "page_cn", "vector-database", "向量数据库", "active")
	_, err := database.Exec(`INSERT INTO source_documents (id, original_name, media_type, sha256, original_path, status, parser_version, created_at) VALUES ('doc_cn', 'cn.txt', 'text/plain', 'hash-cn', 'original', 'parsed', 'v1', '2024-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`INSERT INTO compilation_runs (id, document_id, status, created_at) VALUES ('run_cn', 'doc_cn', 'applied', '2024-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.Exec(`INSERT INTO wiki_claims (id, page_id, text, claim_type, status, created_by_run_id, updated_by_run_id) VALUES ('claim_cn', 'page_cn', '向量数据库系统支持语义检索。', 'fact', 'active', 'run_cn', 'run_cn')`); err != nil {
		t.Fatal(err)
	}
	indexer := indexing.Service{DB: database, Embedder: llm.DeterministicEmbedding{}}
	if _, err := indexer.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := (Service{DB: database, Embedder: llm.DeterministicEmbedding{}}).Search(context.Background(), "向量数据库系统")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Trace.FTSCandidates) == 0 || len(result.Candidates) == 0 || result.Candidates[0].PageID != "page_cn" {
		t.Fatalf("Chinese result = %#v", result)
	}
}
