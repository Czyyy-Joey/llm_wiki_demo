package indexing

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/joeychen/llm-wiki-demo/backend/internal/db"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	chromem "github.com/philippgille/chromem-go"
)

type countingEmbedder struct {
	calls  int
	inputs int
}

func (e *countingEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	e.calls++
	e.inputs += len(inputs)
	return (llm.DeterministicEmbedding{}).Embed(ctx, inputs)
}

func indexingTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(context.Background(), "file:"+filepath.Join(t.TempDir(), "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func TestReindexPersistsChromemAndReusesUnchangedEmbeddings(t *testing.T) {
	ctx := context.Background()
	database := indexingTestDB(t)
	defer database.Close()
	_, err := database.Exec(`
		INSERT INTO wiki_pages (id, slug, page_type, title, summary, status, current_revision, created_at, updated_at)
		VALUES ('page_active', 'vector-databases', 'concept', 'Vector Databases', 'Semantic storage.', 'active', 1, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z'),
		       ('page_merged', 'old-vector-store', 'concept', 'Old Vector Store', 'Deprecated.', 'merged', 1, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z');
		INSERT INTO source_documents (id, original_name, media_type, sha256, original_path, status, parser_version, created_at)
		VALUES ('doc_1', 'vector.txt', 'text/plain', 'hash', 'raw/vector.txt', 'parsed', 'v1', '2024-01-01T00:00:00Z');
		INSERT INTO source_chunks (id, document_id, chunk_index, text, char_start, char_end, content_hash)
		VALUES ('chunk_1', 'doc_1', 0, 'Vector databases support semantic search.', 0, 41, 'chunk-hash');
		INSERT INTO compilation_runs (id, document_id, status, created_at)
		VALUES ('run_1', 'doc_1', 'applied', '2024-01-01T00:00:00Z');
		INSERT INTO wiki_claims (id, page_id, text, claim_type, status, created_by_run_id, updated_by_run_id)
		VALUES ('claim_1', 'page_active', 'Embeddings enable semantic search.', 'fact', 'active', 'run_1', 'run_1'),
		       ('claim_old', 'page_merged', 'This claim must not be indexed.', 'fact', 'active', 'run_1', 'run_1');`)
	if err != nil {
		t.Fatal(err)
	}

	indexDir := filepath.Join(t.TempDir(), "indexes")
	embedder := &countingEmbedder{}
	service := Service{DB: database, Embedder: embedder, Model: "test-embedding-v1", ProviderID: ProviderIdentity("http://ollama-a.example", "test-embedding-v1"), BatchSize: 8, IndexDir: indexDir}
	first, err := service.Reindex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.WikiPassages != 1 || first.SourceChunks != 1 || first.EmbeddingsMade != 2 {
		t.Fatalf("first reindex = %#v", first)
	}
	vectorDB, err := chromem.NewPersistentDB(indexDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := vectorDB.GetCollection(WikiCollection, nil).Count(); got != 1 {
		t.Fatalf("Wiki collection count = %d", got)
	}
	if got := vectorDB.GetCollection(SourceCollection, nil).Count(); got != 1 {
		t.Fatalf("Source collection count = %d", got)
	}
	if _, err := vectorDB.GetCollection(WikiCollection, nil).GetByID(ctx, "passage_"+stableHash("page_merged:page")); err == nil {
		t.Fatal("merged page was written to chromem")
	}

	second, err := service.Reindex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.EmbeddingsMade != 0 || embedder.inputs != 2 {
		t.Fatalf("unchanged reindex = %#v, embedded inputs = %d", second, embedder.inputs)
	}
	if _, err := database.Exec(`UPDATE wiki_claims SET text = 'Embeddings enable semantic and similarity search.' WHERE id = 'claim_1'`); err != nil {
		t.Fatal(err)
	}
	third, err := service.Reindex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if third.EmbeddingsMade != 1 || embedder.inputs != 3 {
		t.Fatalf("incremental reindex = %#v, embedded inputs = %d", third, embedder.inputs)
	}
	service.ProviderID = ProviderIdentity("http://ollama-b.example", service.Model)
	providerChanged, err := service.Reindex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if providerChanged.EmbeddingsMade != 2 || embedder.inputs != 5 {
		t.Fatalf("provider change reused stale vectors: result = %#v, embedded inputs = %d", providerChanged, embedder.inputs)
	}

	if err := os.RemoveAll(indexDir); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := service.Reindex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.EmbeddingsMade != 0 {
		t.Fatalf("directory rebuild recomputed embeddings: %#v", rebuilt)
	}
	vectorDB, err = chromem.NewPersistentDB(indexDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := vectorDB.GetCollection(WikiCollection, nil).Count(); got != 1 {
		t.Fatalf("rebuilt Wiki collection count = %d", got)
	}
	var status string
	if err := database.QueryRow(`SELECT index_status FROM wiki_pages WHERE id = 'page_active'`).Scan(&status); err != nil || status != "clean" {
		t.Fatalf("index status = %q, err = %v", status, err)
	}
}
