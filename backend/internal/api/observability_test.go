package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeychen/llm-wiki-demo/backend/internal/config"
	"github.com/joeychen/llm-wiki-demo/backend/internal/db"
	"github.com/joeychen/llm-wiki-demo/backend/internal/sources"
)

func TestRequestIDAndErrorEnvelope(t *testing.T) {
	database, err := db.Open(context.Background(), "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	server := &Server{DB: database, Config: config.Load(), Sources: sources.Service{DB: database}}
	request := httptest.NewRequest(http.MethodGet, "/api/source-chunks/missing", nil)
	request.Header.Set("X-Request-ID", "req-contract-test")
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || response.Header().Get("X-Request-ID") != "req-contract-test" {
		t.Fatalf("status/request id = %d/%q", response.Code, response.Header().Get("X-Request-ID"))
	}
	var payload struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != "not_found" || payload.Error.Message == "" || payload.Error.RequestID != "req-contract-test" {
		t.Fatalf("error envelope = %#v", payload)
	}

	response = httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("generated request ID is missing")
	}
}

func TestSSEErrorIncludesRequestID(t *testing.T) {
	server, _, cleanup := phase5Server(t)
	defer cleanup()
	request := httptest.NewRequest(http.MethodPost, "/api/conversations/missing/messages/stream", bytes.NewReader([]byte(`{"question":"hello"}`)))
	request.Header.Set("X-Request-ID", "req-sse-test")
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), `"request_id":"req-sse-test"`) {
		t.Fatalf("SSE error body = %s", response.Body.String())
	}
}
