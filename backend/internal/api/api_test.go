package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/joeychen/llm-wiki-demo/backend/internal/compiler"
	"github.com/joeychen/llm-wiki-demo/backend/internal/config"
	"github.com/joeychen/llm-wiki-demo/backend/internal/db"
	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/indexing"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	"github.com/joeychen/llm-wiki-demo/backend/internal/retrieval"
	"github.com/joeychen/llm-wiki-demo/backend/internal/sources"
	"github.com/joeychen/llm-wiki-demo/backend/internal/wiki"
)

func TestHealth(t *testing.T) {
	database, err := db.Open(context.Background(), "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	response := httptest.NewRecorder()
	server := &Server{DB: database, Config: config.Load()}
	server.Router().ServeHTTP(response, httptest.NewRequest("GET", "/api/health", nil))
	if response.Code != 200 {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestSourceUploadListDetailAndChunks(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(context.Background(), "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	server := &Server{DB: database, Config: config.Load(), Sources: sources.Service{DB: database, DataRoot: root}}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "evidence.md")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("# Evidence\n\nTraceable fact."))
	_ = writer.Close()
	request := httptest.NewRequest("POST", "/api/sources", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	if response.Code != 201 {
		t.Fatalf("upload status = %d, body = %s", response.Code, response.Body.String())
	}
	var upload struct {
		Source struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"source"`
		Chunks []struct {
			HeadingPath []string `json:"heading_path"`
		} `json:"chunks"`
		Result struct {
			Action string `json:"action"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &upload); err != nil {
		t.Fatal(err)
	}
	if upload.Source.Status != sources.StatusParsed || upload.Result.Action != "CREATED" || len(upload.Chunks) != 1 || upload.Chunks[0].HeadingPath[0] != "Evidence" {
		t.Fatalf("upload response = %#v", upload)
	}

	for _, path := range []string{"/api/sources", "/api/sources/" + upload.Source.ID, "/api/sources/" + upload.Source.ID + "/chunks"} {
		response = httptest.NewRecorder()
		server.Router().ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 {
			t.Fatalf("GET %s status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}

	body.Reset()
	writer = multipart.NewWriter(&body)
	file, _ = writer.CreateFormFile("file", "renamed.md")
	_, _ = file.Write([]byte("# Evidence\n\nTraceable fact."))
	_ = writer.Close()
	request = httptest.NewRequest("POST", "/api/sources", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response = httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("duplicate upload status = %d, body = %s", response.Code, response.Body.String())
	}
	var duplicate struct {
		Result struct {
			Action string `json:"action"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &duplicate); err != nil || duplicate.Result.Action != "NO_OP" {
		t.Fatalf("duplicate response = %#v, err = %v", duplicate, err)
	}
}

func TestOpenAPIContainsCoreSchemas(t *testing.T) {
	database, err := db.Open(context.Background(), "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	response := httptest.NewRecorder()
	server := &Server{DB: database, Config: config.Load()}
	server.Router().ServeHTTP(response, httptest.NewRequest("GET", "/openapi.json", nil))
	var document struct {
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SourceDocument", "WikiPage", "CompilationPlan", "SourceAnalysis", "CompilationCandidate", "RetrievalTrace", "Conversation", "Message"} {
		if _, ok := document.Components.Schemas[name]; !ok {
			t.Errorf("missing OpenAPI schema %s", name)
		}
	}
}

func TestCompilationAPIExposesIntermediateResults(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(context.Background(), "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	server := &Server{
		DB:       database,
		Config:   config.Load(),
		Sources:  sources.Service{DB: database, DataRoot: root},
		Compiler: compiler.Service{DB: database, DataRoot: root, LLM: llm.DeterministicFake{}},
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "compiler-api.md")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("# API Concept\n\nA claim visible through the compilation API."))
	_ = writer.Close()
	uploadRequest := httptest.NewRequest("POST", "/api/sources", &body)
	uploadRequest.Header.Set("Content-Type", writer.FormDataContentType())
	uploadResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(uploadResponse, uploadRequest)
	if uploadResponse.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", uploadResponse.Code, uploadResponse.Body.String())
	}
	var upload struct {
		Source struct {
			ID string `json:"id"`
		} `json:"source"`
	}
	if err := json.Unmarshal(uploadResponse.Body.Bytes(), &upload); err != nil {
		t.Fatal(err)
	}
	compileBody, _ := json.Marshal(map[string]string{"document_id": upload.Source.ID})
	compileRequest := httptest.NewRequest("POST", "/api/compilations", bytes.NewReader(compileBody))
	compileRequest.Header.Set("Content-Type", "application/json")
	compileResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(compileResponse, compileRequest)
	if compileResponse.Code != http.StatusCreated {
		t.Fatalf("compile status = %d, body = %s", compileResponse.Code, compileResponse.Body.String())
	}
	var result struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(compileResponse.Body.Bytes(), &result); err != nil || result.RunID == "" {
		t.Fatalf("compile response = %s, err = %v", compileResponse.Body.String(), err)
	}
	getResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(getResponse, httptest.NewRequest("GET", "/api/compilations/"+result.RunID, nil))
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get compilation status = %d, body = %s", getResponse.Code, getResponse.Body.String())
	}
	var run map[string]json.RawMessage
	if err := json.Unmarshal(getResponse.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"analyze", "candidates", "plan", "validation", "apply_result", "diff"} {
		if len(run[field]) == 0 {
			t.Errorf("GET compilation missing %s: %s", field, getResponse.Body.String())
		}
	}
}

func TestWikiAPIExposesBrowsingAndProvenanceContracts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := db.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	server := &Server{
		DB:       database,
		Config:   config.Load(),
		Sources:  sources.Service{DB: database, DataRoot: root},
		Compiler: compiler.Service{DB: database, DataRoot: root, LLM: llm.DeterministicFake{}},
		Wiki:     wiki.Service{DB: database},
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "api-wiki.md")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("# API Wiki\n\nA claim exposed through the Wiki service."))
	_ = writer.Close()
	uploadRequest := httptest.NewRequest(http.MethodPost, "/api/sources", &body)
	uploadRequest.Header.Set("Content-Type", writer.FormDataContentType())
	uploadResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(uploadResponse, uploadRequest)
	if uploadResponse.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", uploadResponse.Code, uploadResponse.Body.String())
	}
	var upload struct {
		Source struct {
			ID string `json:"id"`
		} `json:"source"`
	}
	if err := json.Unmarshal(uploadResponse.Body.Bytes(), &upload); err != nil {
		t.Fatal(err)
	}
	compileBody, _ := json.Marshal(map[string]string{"document_id": upload.Source.ID})
	compileRequest := httptest.NewRequest(http.MethodPost, "/api/compilations", bytes.NewReader(compileBody))
	compileRequest.Header.Set("Content-Type", "application/json")
	compileResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(compileResponse, compileRequest)
	if compileResponse.Code != http.StatusCreated {
		t.Fatalf("compile status = %d, body = %s", compileResponse.Code, compileResponse.Body.String())
	}

	var pages struct {
		Pages []struct {
			Slug       string `json:"slug"`
			ClaimCount int    `json:"claim_count"`
		} `json:"pages"`
	}
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/wiki/pages?type=concept", nil))
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &pages) != nil || len(pages.Pages) != 1 || pages.Pages[0].ClaimCount != 1 {
		t.Fatalf("wiki page list status/body = %d/%s", response.Code, response.Body.String())
	}
	pageSlug := pages.Pages[0].Slug
	for _, path := range []string{
		"/api/wiki/pages/" + pageSlug,
		"/api/wiki/pages/" + pageSlug + "/revisions",
		"/api/wiki/pages/" + pageSlug + "/revisions/1",
		"/api/sources/" + upload.Source.ID + "/wiki",
		"/api/wiki/lint",
	} {
		response = httptest.NewRecorder()
		server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
	var detail wiki.PageDetail
	response = httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/wiki/pages/"+pageSlug, nil))
	if err := json.NewDecoder(response.Body).Decode(&detail); err != nil || len(detail.Claims) != 1 || len(detail.Claims[0].Evidence) != 1 {
		t.Fatalf("wiki detail = %#v, err = %v", detail, err)
	}
}

func TestRetrievalAPIReindexSearchAndTrace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := db.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, err = database.Exec(`
		INSERT INTO wiki_pages (id, slug, page_type, title, summary, status, current_revision, created_at, updated_at)
		VALUES ('page_search', 'vector-databases', 'concept', 'Vector Databases', 'Compiled semantic search knowledge.', 'active', 1, '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z');
		INSERT INTO source_documents (id, original_name, media_type, sha256, original_path, status, parser_version, created_at)
		VALUES ('doc_search', 'search.txt', 'text/plain', 'search-hash', 'raw/search.txt', 'parsed', 'v1', '2024-01-01T00:00:00Z');
		INSERT INTO source_chunks (id, document_id, chunk_index, text, char_start, char_end, content_hash)
		VALUES ('chunk_search', 'doc_search', 0, 'Vector databases store embeddings.', 0, 34, 'chunk-search-hash');
		INSERT INTO compilation_runs (id, document_id, status, created_at)
		VALUES ('run_search', 'doc_search', 'applied', '2024-01-01T00:00:00Z');
		INSERT INTO wiki_claims (id, page_id, text, claim_type, status, created_by_run_id, updated_by_run_id)
		VALUES ('claim_search', 'page_search', 'Vector databases store embeddings.', 'fact', 'active', 'run_search', 'run_search');
		INSERT INTO claim_evidence (claim_id, source_chunk_id, relation)
		VALUES ('claim_search', 'chunk_search', 'supports');`)
	if err != nil {
		t.Fatal(err)
	}
	indexDir := filepath.Join(root, "indexes")
	indexer := indexing.Service{DB: database, Embedder: llm.DeterministicEmbedding{}, IndexDir: indexDir}
	server := &Server{
		DB:        database,
		Config:    config.Load(),
		Indexer:   indexer,
		Retriever: retrieval.Service{DB: database, Embedder: llm.DeterministicEmbedding{}, IndexDir: indexDir},
	}

	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/indexes/reindex", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("reindex status = %d, body = %s", response.Code, response.Body.String())
	}
	var indexed indexing.IndexResult
	if err := json.Unmarshal(response.Body.Bytes(), &indexed); err != nil || indexed.WikiPassages != 1 || indexed.SourceChunks != 1 {
		t.Fatalf("reindex response = %#v, err = %v", indexed, err)
	}

	response = httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/retrieval/search?q=semantic+embeddings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("search status = %d, body = %s", response.Code, response.Body.String())
	}
	var result retrieval.Result
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Candidates) != 1 || result.Trace.ID == "" {
		t.Fatalf("search response = %#v, err = %v", result, err)
	}

	response = httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/retrieval/traces/"+result.Trace.ID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("trace status = %d, body = %s", response.Code, response.Body.String())
	}
	var trace domain.RetrievalTrace
	if err := json.Unmarshal(response.Body.Bytes(), &trace); err != nil || trace.ID != result.Trace.ID || len(trace.Candidates) == 0 {
		t.Fatalf("trace response = %#v, err = %v", trace, err)
	}
}
