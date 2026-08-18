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
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	"github.com/joeychen/llm-wiki-demo/backend/internal/sources"
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
