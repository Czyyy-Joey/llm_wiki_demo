package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeychen/llm-wiki-demo/backend/internal/chat"
	"github.com/joeychen/llm-wiki-demo/backend/internal/compiler"
	"github.com/joeychen/llm-wiki-demo/backend/internal/config"
	"github.com/joeychen/llm-wiki-demo/backend/internal/db"
	"github.com/joeychen/llm-wiki-demo/backend/internal/indexing"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	queryservice "github.com/joeychen/llm-wiki-demo/backend/internal/query"
	"github.com/joeychen/llm-wiki-demo/backend/internal/retrieval"
)

func TestSettingsEmbeddingHotUpdateRebindsAllRuntimeServices(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := db.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, err = database.Exec(`INSERT INTO wiki_pages (id, slug, page_type, title, summary, status, current_revision, index_status, created_at, updated_at)
		VALUES ('page-hot-update', 'hot-update', 'concept', 'Hot Update', 'Provider hot update test.', 'active', 1, 'clean', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}

	indexer := indexing.Service{DB: database, Embedder: llm.DeterministicEmbedding{}, IndexDir: filepath.Join(root, "indexes")}
	retriever := retrieval.Service{DB: database, Embedder: llm.DeterministicEmbedding{}, IndexDir: indexer.IndexDir}
	server := &Server{
		DB:        database,
		Config:    config.Load(),
		Compiler:  compiler.Service{DB: database, DataRoot: root, LLM: llm.DeterministicFake{}},
		Indexer:   indexer,
		Retriever: retriever,
		Query:     queryservice.Service{Generator: llm.DeterministicFake{}},
		Chat:      chat.Service{DB: database, Generator: llm.DeterministicFake{}},
	}
	server.BindRuntimeServices()

	settings := map[string]any{
		"llm":       map[string]any{"base_url": "https://oneapi.example/v1", "api_key": "oneapi-key", "model": "oneapi-model"},
		"embedding": map[string]any{"base_url": "http://ollama.example", "model": "embedding-v2", "batch_size": 4},
	}
	body, _ := json.Marshal(settings)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/config/settings", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	server.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("settings status = %d, body = %s", response.Code, response.Body.String())
	}
	var saved struct {
		ReindexRequired bool `json:"reindex_required"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil || !saved.ReindexRequired {
		t.Fatalf("settings response = %s, err = %v", response.Body.String(), err)
	}
	if strings.Contains(response.Body.String(), "oneapi-key") {
		t.Fatalf("settings response leaked API key: %s", response.Body.String())
	}

	newEmbedder, ok := server.Indexer.Embedder.(*llm.OllamaEmbedding)
	if !ok || newEmbedder.Model != "embedding-v2" {
		t.Fatalf("indexer embedder = %#v", server.Indexer.Embedder)
	}
	if server.Indexer.ProviderID != indexing.ProviderIdentity("http://ollama.example", "embedding-v2") {
		t.Fatalf("indexer provider identity = %q", server.Indexer.ProviderID)
	}
	if got, ok := server.Retriever.Embedder.(*llm.OllamaEmbedding); !ok || got != newEmbedder {
		t.Fatal("retriever is not using the indexer's new embedder")
	}
	queryRetriever, ok := server.Query.Retriever.(retrieval.Service)
	if !ok {
		t.Fatalf("query retriever type = %T", server.Query.Retriever)
	}
	if got, ok := queryRetriever.Embedder.(*llm.OllamaEmbedding); !ok || got != newEmbedder {
		t.Fatal("query is not using the new embedder")
	}
	if got, ok := server.Chat.Query.Retriever.(retrieval.Service); !ok {
		t.Fatalf("chat retriever type = %T", server.Chat.Query.Retriever)
	} else if embedder, ok := got.Embedder.(*llm.OllamaEmbedding); !ok || embedder != newEmbedder {
		t.Fatal("chat is not using the new embedder")
	}
	if server.Compiler.Index != &server.Indexer || server.Compiler.Index.Embedder != newEmbedder {
		t.Fatal("compiler is not bound to the shared indexer")
	}
	compilerLLM, compilerOK := server.Compiler.LLM.(*llm.OpenAICompatible)
	chatLLM, chatOK := server.Query.Generator.(*llm.OpenAICompatible)
	if !compilerOK || !chatOK || compilerLLM.Endpoint != "https://oneapi.example/v1" || chatLLM.Endpoint != compilerLLM.Endpoint || chatLLM.Model != compilerLLM.Model {
		t.Fatalf("OneAPI clients drifted: compiler=%#v chat=%#v", server.Compiler.LLM, server.Query.Generator)
	}
	if server.Chat.Generator != server.Query.Generator {
		t.Fatal("chat and query do not share the updated OneAPI client")
	}

	var status string
	if err := database.QueryRow(`SELECT index_status FROM wiki_pages WHERE id = 'page-hot-update'`).Scan(&status); err != nil || status != "index_pending" {
		t.Fatalf("index status after provider change = %q, err = %v", status, err)
	}

	statusResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/api/config/status", nil))
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", statusResponse.Code, statusResponse.Body.String())
	}
	if strings.Contains(statusResponse.Body.String(), "oneapi-key") {
		t.Fatalf("config status leaked API key: %s", statusResponse.Body.String())
	}
	if !strings.Contains(statusResponse.Body.String(), `"api_key_set":true`) {
		t.Fatalf("config status did not report configured key: %s", statusResponse.Body.String())
	}
}
