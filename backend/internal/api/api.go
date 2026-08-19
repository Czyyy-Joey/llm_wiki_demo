package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	chatservice "github.com/joeychen/llm-wiki-demo/backend/internal/chat"
	"github.com/joeychen/llm-wiki-demo/backend/internal/compiler"
	"github.com/joeychen/llm-wiki-demo/backend/internal/config"
	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/indexing"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	queryservice "github.com/joeychen/llm-wiki-demo/backend/internal/query"
	"github.com/joeychen/llm-wiki-demo/backend/internal/retrieval"
	"github.com/joeychen/llm-wiki-demo/backend/internal/sources"
	"github.com/joeychen/llm-wiki-demo/backend/internal/wiki"
)

type Server struct {
	mu        sync.RWMutex
	DB        *sql.DB
	Config    config.Config
	Sources   sources.Service
	Compiler  compiler.Service
	Wiki      wiki.Service
	Indexer   indexing.Service
	Retriever retrieval.Service
	Query     queryservice.Service
	Chat      chatservice.Service
}

// BindRuntimeServices keeps the compiler, indexer, retriever, query, and chat
// on one provider graph. Service values are intentionally small structs, but
// their runtime adapters must not drift after a settings update.
func (s *Server) BindRuntimeServices() {
	s.Compiler.Index = &s.Indexer
	s.Query.Retriever = s.Retriever
	s.Chat.Query = s.Query
}

type HealthResponse struct {
	Body struct {
		Status    string                `json:"status"`
		Database  string                `json:"database"`
		Providers config.ProviderStatus `json:"providers"`
	}
}
type ConfigResponse struct {
	Body struct {
		Providers config.ProviderStatus `json:"providers"`
		Settings  PublicSettings        `json:"settings"`
	}
}
type PublicLLMSettings struct {
	Endpoint     string `json:"base_url"`
	Model        string `json:"model"`
	APIKeySet    bool   `json:"api_key_set"`
	FakeFallback bool   `json:"fake_fallback,omitempty"`
}
type PublicEmbeddingSettings struct {
	Endpoint  string `json:"base_url"`
	Model     string `json:"model"`
	BatchSize int    `json:"batch_size,omitempty"`
}
type PublicSettings struct {
	LLM       PublicLLMSettings       `json:"llm"`
	Embedding PublicEmbeddingSettings `json:"embedding"`
}
type ErrorResponse struct {
	Body struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id,omitempty"`
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	apiConfig := huma.DefaultConfig("LLM-Wiki API", "0.1.0")
	registerCoreSchemas(apiConfig.OpenAPI.Components.Schemas)
	api := humachi.New(r, apiConfig)
	huma.Register(api, huma.Operation{OperationID: "health", Method: http.MethodGet, Path: "/api/health", Summary: "Application and database health"}, func(ctx context.Context, input *struct{}) (*HealthResponse, error) { return s.health() })
	huma.Register(api, huma.Operation{OperationID: "config-status", Method: http.MethodGet, Path: "/api/config/status", Summary: "Configured provider status"}, func(ctx context.Context, input *struct{}) (*ConfigResponse, error) {
		out := &ConfigResponse{}
		s.mu.RLock()
		out.Body.Providers = s.Config.ProviderStatus()
		out.Body.Settings = publicSettings(s.Config)
		s.mu.RUnlock()
		return out, nil
	})
	r.Put("/api/config/settings", s.updateSettings)
	r.Post("/api/sources", s.uploadSource)
	r.Get("/api/sources", s.listSources)
	r.Get("/api/source-chunks/{id}", s.getSourceChunk)
	r.Get("/api/sources/{id}", s.getSource)
	r.Get("/api/sources/{id}/chunks", s.getSourceChunks)
	r.Get("/api/sources/{id}/wiki", s.getSourceWiki)
	r.Get("/api/wiki/pages", s.listWikiPages)
	r.Get("/api/wiki/pages/{key}", s.getWikiPage)
	r.Get("/api/wiki/pages/{key}/revisions", s.getWikiRevisions)
	r.Get("/api/wiki/pages/{key}/revisions/{revision}", s.getWikiRevisionDiff)
	r.Get("/api/wiki/lint", s.lintWiki)
	r.Post("/api/compilations", s.createCompilation)
	r.Get("/api/compilations", s.listCompilations)
	r.Get("/api/compilations/{id}", s.getCompilation)
	r.Post("/api/compilations/{id}/render", s.retryCompilationRender)
	r.Post("/api/indexes/reindex", s.reindex)
	r.Get("/api/retrieval/search", s.search)
	r.Get("/api/retrieval/traces/{id}", s.getRetrievalTrace)
	r.Post("/api/query", s.query)
	r.Post("/api/conversations", s.createConversation)
	r.Get("/api/conversations", s.listConversations)
	r.Get("/api/conversations/{id}", s.getConversation)
	r.Delete("/api/conversations/{id}", s.deleteConversation)
	r.Post("/api/conversations/{id}/messages", s.sendMessage)
	r.Post("/api/conversations/{id}/messages/stream", s.streamMessage)
	return requestID(r)
}

func (s *Server) reindex(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	indexer := s.Indexer
	s.mu.RUnlock()
	result, err := indexer.Reindex(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	retriever := s.Retriever
	s.mu.RUnlock()
	result, err := retriever.Search(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) getRetrievalTrace(w http.ResponseWriter, r *http.Request) {
	var trace domain.RetrievalTrace
	var raw string
	err := s.DB.QueryRowContext(r.Context(), `SELECT trace_json FROM retrieval_traces WHERE id = ?`, chi.URLParam(r, "id")).Scan(&raw)
	if err != nil {
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("retrieval trace not found"))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if err := json.Unmarshal([]byte(raw), &trace); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, trace)
}

type questionRequest struct {
	Question string `json:"question"`
}

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	var request questionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("question is required"))
		return
	}
	s.mu.RLock()
	service := s.Query
	s.mu.RUnlock()
	result, err := service.Ask(r.Context(), request.Question)
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type conversationRequest struct {
	Title string `json:"title"`
}

func (s *Server) createConversation(w http.ResponseWriter, r *http.Request) {
	var request conversationRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&request)
	}
	conversation, err := s.Chat.Create(r.Context(), request.Title)
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusCreated, conversation)
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	items, err := s.Chat.List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": items})
}

func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) {
	detail, err := s.Chat.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("conversation not found"))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) deleteConversation(w http.ResponseWriter, r *http.Request) {
	if err := s.Chat.Delete(r.Context(), chi.URLParam(r, "id")); err != nil {
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("conversation not found"))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var request questionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("question is required"))
		return
	}
	s.mu.RLock()
	service := s.Chat
	s.mu.RUnlock()
	result, err := service.Send(r.Context(), chi.URLParam(r, "id"), request.Question)
	if err != nil {
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("conversation not found"))
			return
		}
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) streamMessage(w http.ResponseWriter, r *http.Request) {
	var request questionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeSSEError(w, http.StatusBadRequest, fmt.Errorf("question is required"))
		return
	}
	s.mu.RLock()
	service := s.Chat
	s.mu.RUnlock()
	result, err := service.Send(r.Context(), chi.URLParam(r, "id"), request.Question)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if err == sql.ErrNoRows {
			status = http.StatusNotFound
		}
		writeSSEError(w, status, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, runeValue := range []rune(result.AssistantMessage.Content) {
		payload, _ := json.Marshal(map[string]string{"delta": string(runeValue), "message_id": result.AssistantMessage.ID})
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	done, _ := json.Marshal(result)
	_, _ = fmt.Fprintf(w, "event: done\ndata: %s\n\n", done)
	if flusher != nil {
		flusher.Flush()
	}
}

func writeSSEError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(status)
	payload, _ := json.Marshal(map[string]string{"error": err.Error()})
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
}

type compilationRequest struct {
	DocumentID string `json:"document_id"`
}

func (s *Server) createCompilation(w http.ResponseWriter, r *http.Request) {
	var request compilationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.DocumentID == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("document_id is required"))
		return
	}
	s.mu.RLock()
	service := s.Compiler
	s.mu.RUnlock()
	result, err := service.Compile(r.Context(), request.DocumentID)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, result)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) listCompilations(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.Compiler.ListRuns(r.Context(), limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"compilations": runs})
}

func (s *Server) getCompilation(w http.ResponseWriter, r *http.Request) {
	run, err := s.Compiler.GetRun(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("compilation run not found"))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) retryCompilationRender(w http.ResponseWriter, r *http.Request) {
	if err := s.Compiler.RetryRender(r.Context(), chi.URLParam(r, "id")); err != nil {
		var pending *compiler.RenderPendingError
		if errors.As(err, &pending) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"status": compiler.RunRenderPending, "error": err.Error()})
			return
		}
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("compilation run not found"))
			return
		}
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	run, err := s.Compiler.GetRun(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) uploadSource(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("file field is required"))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 64<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("read source file: %w", err))
		return
	}
	result, err := s.Sources.Ingest(r.Context(), sources.IngestInput{OriginalName: header.Filename, MediaType: header.Header.Get("Content-Type"), Data: data})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	status := http.StatusCreated
	if result.NoOp {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"source": result.Document, "chunks": result.Chunks, "result": map[string]any{"action": map[bool]string{true: "NO_OP", false: "CREATED"}[result.NoOp]}})
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	result, err := s.Sources.List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": result})
}
func (s *Server) getSource(w http.ResponseWriter, r *http.Request) {
	item, err := s.Sources.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}
func (s *Server) getSourceChunks(w http.ResponseWriter, r *http.Request) {
	result, err := s.Sources.Chunks(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chunks": result})
}
func (s *Server) getSourceChunk(w http.ResponseWriter, r *http.Request) {
	result, err := s.Sources.Chunk(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("source chunk not found"))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) getSourceWiki(w http.ResponseWriter, r *http.Request) {
	result, err := s.Wiki.SourceTrace(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("source not found"))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) listWikiPages(w http.ResponseWriter, r *http.Request) {
	result, err := s.Wiki.ListPages(r.Context(), r.URL.Query().Get("type"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pages": result})
}
func (s *Server) getWikiPage(w http.ResponseWriter, r *http.Request) {
	result, err := s.Wiki.GetPage(r.Context(), chi.URLParam(r, "key"))
	if err != nil {
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("wiki page not found"))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) getWikiRevisions(w http.ResponseWriter, r *http.Request) {
	result, err := s.Wiki.Revisions(r.Context(), chi.URLParam(r, "key"))
	if err != nil {
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("wiki page not found"))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": result})
}
func (s *Server) getWikiRevisionDiff(w http.ResponseWriter, r *http.Request) {
	revision, err := strconv.Atoi(chi.URLParam(r, "revision"))
	if err != nil || revision < 1 {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("revision must be a positive integer"))
		return
	}
	result, err := s.Wiki.Diff(r.Context(), chi.URLParam(r, "key"), revision)
	if err != nil {
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("revision not found"))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) lintWiki(w http.ResponseWriter, r *http.Request) {
	issues, err := s.Wiki.Lint(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": issues, "valid": len(issues) == 0})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeJSONError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func registerCoreSchemas(registry huma.Registry) {
	for _, contract := range []any{
		domain.SourceDocument{}, domain.SourceChunk{}, domain.WikiPage{},
		domain.WikiSection{}, domain.WikiClaim{}, domain.ClaimEvidence{}, domain.WikiLink{}, domain.WikiRevision{},
		domain.CompilationPlan{}, domain.RetrievalTrace{},
		domain.SourceAnalysis{}, domain.AnalyzedTopic{}, domain.AnalyzedClaim{}, domain.CompilationCandidate{},
		domain.Conversation{}, domain.Message{}, domain.CitationSnapshot{}, domain.StandaloneQuery{}, domain.GeneratedAnswer{},
	} {
		registry.Schema(reflect.TypeOf(contract), true, "")
	}
}

func (s *Server) health() (*HealthResponse, error) {
	out := &HealthResponse{}
	out.Body.Status = "ok"
	out.Body.Database = "ok"
	s.mu.RLock()
	out.Body.Providers = s.Config.ProviderStatus()
	s.mu.RUnlock()
	if err := s.DB.Ping(); err != nil {
		out.Body.Status = "degraded"
		out.Body.Database = "error"
	}
	return out, nil
}

type llmSettingsInput struct {
	Endpoint     string `json:"base_url"`
	APIKey       string `json:"api_key"`
	Model        string `json:"model"`
	FakeFallback bool   `json:"fake_fallback"`
}
type embeddingSettingsInput struct {
	Endpoint  string `json:"base_url"`
	Model     string `json:"model"`
	BatchSize int    `json:"batch_size"`
}
type settingsInput struct {
	LLM       llmSettingsInput       `json:"llm"`
	Embedding embeddingSettingsInput `json:"embedding"`
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var input settingsInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid settings payload"))
		return
	}
	for name, endpoint := range map[string]string{"llm": input.LLM.Endpoint, "embedding": input.Embedding.Endpoint} {
		if endpoint == "" {
			continue
		}
		parsed, err := url.ParseRequestURI(endpoint)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("%s endpoint must be an HTTP(S) URL", name))
			return
		}
	}
	s.mu.Lock()
	previous := s.Config
	next := s.Config
	llmBaseURL, llmModel := strings.TrimSpace(input.LLM.Endpoint), strings.TrimSpace(input.LLM.Model)
	next.CompilerEndpoint, next.CompilerModel, next.CompilerFakeFallback = llmBaseURL, llmModel, input.LLM.FakeFallback
	next.ChatEndpoint, next.ChatModel, next.ChatFakeFallback = llmBaseURL, llmModel, input.LLM.FakeFallback
	next.EmbeddingEndpoint, next.EmbeddingModel = strings.TrimSpace(input.Embedding.Endpoint), strings.TrimSpace(input.Embedding.Model)
	if input.LLM.APIKey != "" {
		next.CompilerLLMKey, next.ChatLLMKey = input.LLM.APIKey, input.LLM.APIKey
	}
	if input.Embedding.BatchSize > 0 {
		next.EmbeddingBatchSize = input.Embedding.BatchSize
	}
	values := storedSettings(next)
	if err := config.SaveStored(r.Context(), s.DB, values); err != nil {
		s.mu.Unlock()
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	embeddingChanged := embeddingConfigChanged(previous, next)
	s.applyProviderConfig(next)
	if embeddingChanged {
		if _, err := s.DB.ExecContext(r.Context(), `UPDATE wiki_pages SET index_status = 'index_pending' WHERE status = 'active'`); err != nil {
			s.mu.Unlock()
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
	}
	settings, providers := publicSettings(s.Config), s.Config.ProviderStatus()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings, "providers": providers, "reindex_required": embeddingChanged})
}

func (s *Server) applyProviderConfig(next config.Config) {
	var compilerLLM llm.LLMClient
	if next.CompilerLLMConfigured() {
		compilerLLM = llm.NewOpenAICompatible(next.CompilerEndpoint, next.CompilerLLMKey, next.CompilerModel, next.CompilerPromptDir)
	} else if next.CompilerFakeFallback {
		compilerLLM = llm.DeterministicFake{}
	}
	var chatLLM llm.GenerationClient
	if next.ChatLLMConfigured() {
		chatLLM = llm.NewOpenAICompatible(next.ChatEndpoint, next.ChatLLMKey, next.ChatModel, next.ChatPromptDir)
	} else if next.ChatFakeFallback {
		chatLLM = llm.DeterministicFake{}
	}
	embedder := llm.EmbeddingClient(llm.NewOllamaEmbedding(next.EmbeddingEndpoint, next.EmbeddingModel))
	s.Config = next
	s.Compiler.LLM = compilerLLM
	s.Indexer.Embedder, s.Indexer.Model, s.Indexer.ProviderID, s.Indexer.BatchSize = embedder, next.EmbeddingModel, indexing.ProviderIdentity(next.EmbeddingEndpoint, next.EmbeddingModel), next.EmbeddingBatchSize
	s.Retriever.Embedder = embedder
	s.Query.Generator = chatLLM
	s.Chat.Generator = chatLLM
	s.BindRuntimeServices()
}

func embeddingConfigChanged(previous, next config.Config) bool {
	return previous.EmbeddingEndpoint != next.EmbeddingEndpoint || previous.EmbeddingModel != next.EmbeddingModel
}

func publicSettings(c config.Config) PublicSettings {
	return PublicSettings{
		LLM:       PublicLLMSettings{Endpoint: c.CompilerEndpoint, Model: c.CompilerModel, APIKeySet: c.CompilerLLMKey != "", FakeFallback: c.CompilerFakeFallback},
		Embedding: PublicEmbeddingSettings{Endpoint: c.EmbeddingEndpoint, Model: c.EmbeddingModel, BatchSize: c.EmbeddingBatchSize},
	}
}

func storedSettings(c config.Config) map[string]string {
	return map[string]string{
		"llm_base_url": c.CompilerEndpoint, "llm_api_key": c.CompilerLLMKey, "llm_model": c.CompilerModel, "llm_fake_fallback": strconv.FormatBool(c.CompilerFakeFallback),
		"ollama_base_url": c.EmbeddingEndpoint, "embedding_model": c.EmbeddingModel, "embedding_batch_size": strconv.Itoa(c.EmbeddingBatchSize),
	}
}
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			var bytes [12]byte
			if _, err := rand.Read(bytes[:]); err == nil {
				id = hex.EncodeToString(bytes[:])
			} else {
				id = "request-local"
			}
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}
