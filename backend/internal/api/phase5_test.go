package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeychen/llm-wiki-demo/backend/internal/chat"
	"github.com/joeychen/llm-wiki-demo/backend/internal/config"
	"github.com/joeychen/llm-wiki-demo/backend/internal/db"
	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	queryservice "github.com/joeychen/llm-wiki-demo/backend/internal/query"
	"github.com/joeychen/llm-wiki-demo/backend/internal/retrieval"
)

type phase5Retriever struct {
	db    *sql.DB
	calls int
}

func (r *phase5Retriever) Search(_ context.Context, query string) (retrieval.Result, error) {
	r.calls++
	trace := domain.RetrievalTrace{ID: "phase5-trace-" + strings.ReplaceAll(query, " ", "-"), NormalizedQuery: query}
	raw, _ := json.Marshal(trace)
	if _, err := r.db.Exec(`INSERT INTO retrieval_traces (id, normalized_query, trace_json, created_at) VALUES (?, ?, ?, '2024-01-01T00:00:00Z')`, trace.ID, query, string(raw)); err != nil {
		return retrieval.Result{}, err
	}
	return retrieval.Result{Context: []retrieval.ContextItem{
		{ID: "wiki-passage-1", Kind: "wiki", PageID: "page-1", PassageID: "passage-1", Text: "Vector databases organize embeddings for similarity search.", Citation: "vector-databases"},
		{ID: "source-chunk-1", Kind: "source_evidence", PageID: "page-1", SourceChunkID: "source-chunk-1", Text: "Vector databases organize embeddings for similarity search.", Citation: "source-chunk-1"},
	}, Trace: trace}, nil
}

func phase5Server(t *testing.T) (*Server, *phase5Retriever, func()) {
	t.Helper()
	database, err := db.Open(context.Background(), "file:"+filepath.Join(t.TempDir(), "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	retriever := &phase5Retriever{db: database}
	generator := llm.DeterministicFake{}
	sharedQuery := queryservice.Service{Retriever: retriever, Generator: generator}
	return &Server{DB: database, Config: config.Load(), Query: sharedQuery, Chat: chat.Service{DB: database, Query: sharedQuery, Generator: generator}}, retriever, func() { _ = database.Close() }
}

func TestQueryChatAndSSEContracts(t *testing.T) {
	server, retriever, cleanup := phase5Server(t)
	defer cleanup()

	body, _ := json.Marshal(map[string]string{"question": "What are vector databases?"})
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("query status = %d, body = %s", response.Code, response.Body.String())
	}
	var queryResult struct {
		Answer    string                    `json:"answer"`
		Citations []domain.CitationSnapshot `json:"citations"`
		Trace     domain.RetrievalTrace     `json:"retrieval_trace"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &queryResult); err != nil || queryResult.Answer == "" || len(queryResult.Citations) != 1 || queryResult.Citations[0].ID != "source-chunk-1" || queryResult.Citations[0].SourceChunkID != "source-chunk-1" || queryResult.Trace.ID == "" {
		t.Fatalf("query result = %#v, err = %v", queryResult, err)
	}

	response = httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations", bytes.NewReader([]byte(`{"title":"Research"}`))))
	if response.Code != http.StatusCreated {
		t.Fatalf("conversation status = %d, body = %s", response.Code, response.Body.String())
	}
	var conversation domain.Conversation
	if err := json.Unmarshal(response.Body.Bytes(), &conversation); err != nil || conversation.ID == "" {
		t.Fatal(err)
	}

	messagePath := "/api/conversations/" + conversation.ID + "/messages"
	response = httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodPost, messagePath, bytes.NewReader([]byte(`{"question":"What about it?"}`))))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "retrieval_trace") || !strings.Contains(response.Body.String(), `"id":"source-chunk-1"`) {
		t.Fatalf("message status/body = %d/%s", response.Code, response.Body.String())
	}

	streamPath := "/api/conversations/" + conversation.ID + "/messages/stream"
	response = httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodPost, streamPath, bytes.NewReader([]byte(`{"question":"Explain embeddings"}`))))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(response.Body.String(), "event: message") || !strings.Contains(response.Body.String(), "event: done") {
		t.Fatalf("SSE status/headers/body = %d/%s/%s", response.Code, response.Header(), response.Body.String())
	}

	response = httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/"+conversation.ID, nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "standalone_query") || !strings.Contains(response.Body.String(), "retrieval_trace") {
		t.Fatalf("conversation recovery status/body = %d/%s", response.Code, response.Body.String())
	}
	if retriever.calls != 3 {
		t.Fatalf("shared retriever calls = %d, want 3", retriever.calls)
	}
}

func TestSSEReturnsErrorEventForUnknownConversation(t *testing.T) {
	server, _, cleanup := phase5Server(t)
	defer cleanup()
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/missing/messages/stream", bytes.NewReader([]byte(`{"question":"hello"}`))))
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "event: error") {
		t.Fatalf("SSE error status/body = %d/%s", response.Code, response.Body.String())
	}
}
