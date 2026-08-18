package sources

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	appdb "github.com/joeychen/llm-wiki-demo/backend/internal/db"
)

func TestIngestDuplicateAndStableChunkIDs(t *testing.T) {
	input := IngestInput{OriginalName: "guide.md", MediaType: "application/octet-stream", Data: []byte("# Guide\n\nFirst fact.\n\n## Detail\n\nSecond fact.")}
	firstService, closeFirst := testService(t)
	defer closeFirst()
	first, err := firstService.Ingest(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Document.Status != StatusParsed || first.Document.MediaType != "text/markdown" || len(first.Chunks) != 2 {
		t.Fatalf("unexpected first result: %#v", first)
	}
	if _, err := os.Stat(first.Document.OriginalPath); err != nil {
		t.Fatalf("original not preserved: %v", err)
	}
	if _, err := os.Stat(first.Document.ParsedPath); err != nil {
		t.Fatalf("parsed projection not saved: %v", err)
	}

	duplicate, err := firstService.Ingest(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.NoOp || duplicate.Document.ID != first.Document.ID || len(duplicate.Chunks) != len(first.Chunks) {
		t.Fatalf("duplicate result: %#v", duplicate)
	}
	renamedDuplicate, err := firstService.Ingest(context.Background(), IngestInput{OriginalName: "renamed.md", Data: input.Data})
	if err != nil || !renamedDuplicate.NoOp {
		t.Fatalf("renamed duplicate result = %#v, err = %v", renamedDuplicate, err)
	}
	orphanPath := filepath.Join(firstService.DataRoot, "sources", "original", first.Document.SHA256+"_renamed.md")
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("duplicate upload left orphan raw file %s", orphanPath)
	}
	var sourceCount int
	if err := firstService.DB.QueryRow("SELECT COUNT(*) FROM source_documents").Scan(&sourceCount); err != nil || sourceCount != 1 {
		t.Fatalf("source count = %d, err = %v", sourceCount, err)
	}

	secondService, closeSecond := testService(t)
	defer closeSecond()
	second, err := secondService.Ingest(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for i := range first.Chunks {
		if first.Chunks[i].ID != second.Chunks[i].ID {
			t.Fatalf("chunk %d ID is not stable: %s != %s", i, first.Chunks[i].ID, second.Chunks[i].ID)
		}
	}
}

func TestConcurrentIngests(t *testing.T) {
	service, closeService := testService(t)
	defer closeService()

	const count = 4
	var wait sync.WaitGroup
	errors := make(chan error, count)
	for i := 0; i < count; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			result, err := service.Ingest(context.Background(), IngestInput{
				OriginalName: fmt.Sprintf("concurrent-%d.txt", index),
				MediaType:    "text/plain",
				Data:         []byte(fmt.Sprintf("distinct concurrent source %d", index)),
			})
			if err != nil {
				errors <- err
				return
			}
			if result.Document.Status != StatusParsed || len(result.Chunks) != 1 {
				errors <- fmt.Errorf("unexpected ingest result %d: %#v", index, result)
			}
		}(i)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if strings.Contains(strings.ToUpper(err.Error()), "SQLITE_BUSY") || strings.Contains(strings.ToLower(err.Error()), "database is locked") {
			t.Fatalf("concurrent ingest hit SQLite contention: %v", err)
		}
		t.Errorf("concurrent ingest failed: %v", err)
	}

	var sourceCount, chunkCount int
	if err := service.DB.QueryRow("SELECT COUNT(*) FROM source_documents").Scan(&sourceCount); err != nil {
		t.Fatal(err)
	}
	if err := service.DB.QueryRow("SELECT COUNT(*) FROM source_chunks").Scan(&chunkCount); err != nil {
		t.Fatal(err)
	}
	if sourceCount != count || chunkCount != count {
		t.Fatalf("source/chunk count = %d/%d, want %d/%d", sourceCount, chunkCount, count, count)
	}
}

func TestFailedParsePreservesSourceWithoutChunks(t *testing.T) {
	service, closeService := testService(t)
	defer closeService()
	result, err := service.Ingest(context.Background(), IngestInput{OriginalName: "unsupported.docx", Data: []byte("ground truth")})
	if err != nil {
		t.Fatal(err)
	}
	if result.Document.Status != StatusFailed || !strings.Contains(result.Document.ParseError, "unsupported media type") {
		t.Fatalf("failed source = %#v", result.Document)
	}
	if result.Document.ParsedPath != "" || len(result.Chunks) != 0 {
		t.Fatalf("failed parse left parsed data: %#v", result)
	}
	if data, err := os.ReadFile(result.Document.OriginalPath); err != nil || string(data) != "ground truth" {
		t.Fatalf("preserved original = %q, err = %v", data, err)
	}
	var chunkCount int
	if err := service.DB.QueryRow("SELECT COUNT(*) FROM source_chunks WHERE document_id = ?", result.Document.ID).Scan(&chunkCount); err != nil || chunkCount != 0 {
		t.Fatalf("failed source chunk count = %d, err = %v", chunkCount, err)
	}
}

func testService(t *testing.T) (Service, func()) {
	t.Helper()
	root := t.TempDir()
	database, err := appdb.Open(context.Background(), "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	return Service{DB: database, DataRoot: root}, func() { _ = database.Close() }
}
