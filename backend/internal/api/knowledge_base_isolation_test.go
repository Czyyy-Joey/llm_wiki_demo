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

	"github.com/joeychen/llm-wiki-demo/backend/internal/config"
	"github.com/joeychen/llm-wiki-demo/backend/internal/db"
	"github.com/joeychen/llm-wiki-demo/backend/internal/knowledgebase"
)

func TestKnowledgeBasesIsolateSourcesWikiAndConversations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := db.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	server := &Server{DB: database, Config: config.Load()}
	kbs := knowledgebase.Service{DB: database}
	kbA, err := kbs.Create(ctx, "Workspace A", "A", knowledgebase.LanguageZH)
	if err != nil {
		t.Fatal(err)
	}
	kbB, err := kbs.Create(ctx, "Workspace B", "B", knowledgebase.LanguageEN)
	if err != nil {
		t.Fatal(err)
	}

	content := []byte("# Shared concept\n\nThis source exists independently in both knowledge bases.")
	sourceA := uploadTestSource(t, server, kbA.ID, "shared.md", content)
	sourceB := uploadTestSource(t, server, kbB.ID, "renamed.md", content)
	if sourceA == sourceB {
		t.Fatalf("source IDs should be scoped, got %q in both KBs", sourceA)
	}
	assertSourceCount(t, server, kbA.ID, 1)
	assertSourceCount(t, server, kbB.ID, 1)
	assertStatus(t, server, kbB.ID, "/api/sources/"+sourceA, http.StatusNotFound)

	for _, item := range []struct {
		id string
		kb string
	}{
		{id: "page-a", kb: kbA.ID},
		{id: "page-b", kb: kbB.ID},
	} {
		_, err = database.Exec(`INSERT INTO wiki_pages (id, knowledge_base_id, slug, page_type, title, summary, status, current_revision, index_status, created_at, updated_at)
			VALUES (?, ?, 'shared-concept', 'concept', ?, 'Scoped page', 'active', 1, 'clean', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`, item.id, item.kb, item.kb)
		if err != nil {
			t.Fatal(err)
		}
	}
	assertWikiSlug(t, server, kbA.ID, "page-a")
	assertWikiSlug(t, server, kbB.ID, "page-b")
	assertStatus(t, server, kbB.ID, "/api/wiki/pages/page-a", http.StatusNotFound)

	conversationA := createTestConversation(t, server, kbA.ID)
	conversationB := createTestConversation(t, server, kbB.ID)
	if conversationA == conversationB {
		t.Fatalf("conversation IDs should be unique, got %q", conversationA)
	}
	assertStatus(t, server, kbB.ID, "/api/conversations/"+conversationA, http.StatusNotFound)
}

func TestKnowledgeBaseProviderAndIndexStatusAreScoped(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := db.Open(ctx, "file:"+filepath.Join(root, "app.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	server := &Server{DB: database, Config: config.Load()}
	kbs := knowledgebase.Service{DB: database}
	kbA, err := kbs.Create(ctx, "Provider A", "", knowledgebase.LanguageZH)
	if err != nil {
		t.Fatal(err)
	}
	kbB, err := kbs.Create(ctx, "Provider B", "", knowledgebase.LanguageEN)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id string
		kb string
	}{
		{id: "provider-page-a", kb: kbA.ID},
		{id: "provider-page-b", kb: kbB.ID},
	} {
		_, err = database.Exec(`INSERT INTO wiki_pages (id, knowledge_base_id, slug, page_type, title, summary, status, current_revision, index_status, created_at, updated_at)
			VALUES (?, ?, ?, 'concept', ?, 'Provider scope', 'active', 1, 'clean', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`, item.id, item.kb, item.id, item.id)
		if err != nil {
			t.Fatal(err)
		}
	}

	body, _ := json.Marshal(map[string]any{
		"llm":       map[string]any{"base_url": "https://oneapi.example/v1", "api_key": "kb-a-secret", "model": "kb-a-model"},
		"embedding": map[string]any{"base_url": "http://ollama-a.example", "model": "embedding-a", "batch_size": 4},
		"language":  "zh",
	})
	request := httptest.NewRequest(http.MethodPut, "/api/config/settings", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(knowledgeBaseHeader, kbA.ID)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("settings status = %d, body = %s", response.Code, response.Body.String())
	}
	if bytes.Contains(response.Body.Bytes(), []byte("kb-a-secret")) {
		t.Fatalf("settings response leaked the KB-scoped API key: %s", response.Body.String())
	}
	var statusA string
	if err := database.QueryRow(`SELECT index_status FROM wiki_pages WHERE id = 'provider-page-a'`).Scan(&statusA); err != nil || statusA != "index_pending" {
		t.Fatalf("KB A index status = %q, err = %v", statusA, err)
	}
	var statusB string
	if err := database.QueryRow(`SELECT index_status FROM wiki_pages WHERE id = 'provider-page-b'`).Scan(&statusB); err != nil || statusB != "clean" {
		t.Fatalf("KB B index status = %q, err = %v", statusB, err)
	}

	configA := getTestConfig(t, server, kbA.ID)
	if configA["embedding_base_url"] != "http://ollama-a.example" || configA["embedding_model"] != "embedding-a" || configA["language"] != knowledgebase.LanguageZH {
		t.Fatalf("KB A effective config = %#v", configA)
	}
	configB := getTestConfig(t, server, kbB.ID)
	if configB["embedding_base_url"] == "http://ollama-a.example" || configB["embedding_model"] == "embedding-a" || configB["language"] != knowledgebase.LanguageEN {
		t.Fatalf("KB B inherited KB A config = %#v", configB)
	}
}

func uploadTestSource(t *testing.T, server *Server, knowledgeBaseID, name string, content []byte) string {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/sources", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set(knowledgeBaseHeader, knowledgeBaseID)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		Source struct {
			ID string `json:"id"`
		} `json:"source"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Source.ID
}

func assertSourceCount(t *testing.T, server *Server, knowledgeBaseID string, want int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	request.Header.Set(knowledgeBaseHeader, knowledgeBaseID)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("source list status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		Sources []any `json:"sources"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != want {
		t.Fatalf("source count = %d, want %d", len(result.Sources), want)
	}
}

func assertWikiSlug(t *testing.T, server *Server, knowledgeBaseID, pageID string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/wiki/pages", nil)
	request.Header.Set(knowledgeBaseHeader, knowledgeBaseID)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("wiki list status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		Pages []struct {
			ID string `json:"id"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Pages) != 1 || result.Pages[0].ID != pageID {
		t.Fatalf("wiki pages = %#v, want page %s", result.Pages, pageID)
	}
}

func createTestConversation(t *testing.T, server *Server, knowledgeBaseID string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/conversations", bytes.NewReader([]byte(`{"title":"Scoped"}`)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(knowledgeBaseHeader, knowledgeBaseID)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("conversation status = %d, body = %s", response.Code, response.Body.String())
	}
	var conversation struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &conversation); err != nil {
		t.Fatal(err)
	}
	return conversation.ID
}

func assertStatus(t *testing.T, server *Server, knowledgeBaseID, path string, want int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set(knowledgeBaseHeader, knowledgeBaseID)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("GET %s status = %d, want %d, body = %s", path, response.Code, want, response.Body.String())
	}
}

func getTestConfig(t *testing.T, server *Server, knowledgeBaseID string) map[string]string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/config/status", nil)
	request.Header.Set(knowledgeBaseHeader, knowledgeBaseID)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("config status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		Settings struct {
			Embedding struct {
				BaseURL string `json:"base_url"`
				Model   string `json:"model"`
			} `json:"embedding"`
			Language string `json:"language"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"embedding_base_url": result.Settings.Embedding.BaseURL,
		"embedding_model":    result.Settings.Embedding.Model,
		"language":           result.Settings.Language,
	}
}
