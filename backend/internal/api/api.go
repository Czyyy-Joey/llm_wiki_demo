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
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	chatservice "github.com/joeychen/llm-wiki-demo/backend/internal/chat"
	"github.com/joeychen/llm-wiki-demo/backend/internal/compiler"
	"github.com/joeychen/llm-wiki-demo/backend/internal/config"
	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/indexing"
	"github.com/joeychen/llm-wiki-demo/backend/internal/knowledgebase"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	"github.com/joeychen/llm-wiki-demo/backend/internal/logging"
	queryservice "github.com/joeychen/llm-wiki-demo/backend/internal/query"
	"github.com/joeychen/llm-wiki-demo/backend/internal/retrieval"
	"github.com/joeychen/llm-wiki-demo/backend/internal/sources"
	"github.com/joeychen/llm-wiki-demo/backend/internal/wiki"
)

type Server struct {
	mu             sync.RWMutex
	DB             *sql.DB
	Config         config.Config
	Sources        sources.Service
	Compiler       compiler.Service
	Wiki           wiki.Service
	Indexer        indexing.Service
	Retriever      retrieval.Service
	Query          queryservice.Service
	Chat           chatservice.Service
	KnowledgeBases knowledgebase.Service
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
	Language  string                  `json:"language"`
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
		runtime, ok := runtimeFromContext(ctx)
		if !ok {
			return nil, fmt.Errorf("knowledge base runtime is unavailable")
		}
		out.Body.Providers = providerStatus(runtime.Effective)
		out.Body.Settings = publicEffectiveSettings(runtime)
		return out, nil
	})
	r.Get("/api/knowledge-bases", s.listKnowledgeBases)
	r.Post("/api/knowledge-bases", s.createKnowledgeBase)
	r.Get("/api/knowledge-bases/{id}", s.getKnowledgeBase)
	r.Put("/api/knowledge-bases/{id}", s.updateKnowledgeBase)
	r.Post("/api/knowledge-bases/{id}/archive", s.archiveKnowledgeBase)
	r.Put("/api/config/settings", s.updateSettings)
	r.Post("/api/sources", s.uploadSource)
	r.Post("/api/sources/delete", s.deleteSources)
	r.Get("/api/sources", s.listSources)
	r.Get("/api/source-chunks/{id}", s.getSourceChunk)
	r.Get("/api/sources/{id}", s.getSource)
	r.Delete("/api/sources/{id}", s.deleteSource)
	r.Get("/api/sources/{id}/chunks", s.getSourceChunks)
	r.Get("/api/sources/{id}/wiki", s.getSourceWiki)
	r.Get("/api/wiki/graph", s.getWikiGraph)
	r.Get("/api/wiki/pages", s.listWikiPages)
	r.Get("/api/wiki/pages/{key}", s.getWikiPage)
	r.Delete("/api/wiki/pages/{key}", s.deleteWikiPage)
	r.Get("/api/wiki/pages/{key}/revisions", s.getWikiRevisions)
	r.Get("/api/wiki/pages/{key}/revisions/{revision}", s.getWikiRevisionDiff)
	r.Get("/api/wiki/lint", s.lintWiki)
	r.Post("/api/compilations", s.createCompilation)
	r.Post("/api/compilations/relink", s.relinkWiki)
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
	return requestID(accessLog(s.knowledgeBaseScope(r)))
}

func (s *Server) reindex(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	indexer := runtime.Indexer
	result, err := indexer.Reindex(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	retriever := runtime.Retriever
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
	err := s.DB.QueryRowContext(r.Context(), `SELECT trace_json FROM retrieval_traces WHERE id = ? AND knowledge_base_id = ?`, chi.URLParam(r, "id"), knowledgebase.IDFromContext(r.Context())).Scan(&raw)
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
	runtime, _ := runtimeFromContext(r.Context())
	service := runtime.Query
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
	runtime, _ := runtimeFromContext(r.Context())
	conversation, err := runtime.Chat.Create(r.Context(), request.Title)
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusCreated, conversation)
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	items, err := runtime.Chat.List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": items})
}

func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	detail, err := runtime.Chat.Get(r.Context(), chi.URLParam(r, "id"))
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
	runtime, _ := runtimeFromContext(r.Context())
	if err := runtime.Chat.Delete(r.Context(), chi.URLParam(r, "id")); err != nil {
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
	runtime, _ := runtimeFromContext(r.Context())
	service := runtime.Chat
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
	runtime, _ := runtimeFromContext(r.Context())
	service := runtime.Chat
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
	errorValue := errorEnvelope(w, status, err)
	w.WriteHeader(status)
	payload, _ := json.Marshal(map[string]any{"error": errorValue})
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
	runtime, _ := runtimeFromContext(r.Context())
	service := runtime.Compiler
	result, err := service.Compile(r.Context(), request.DocumentID)
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	summary, err := runtime.Compiler.DeleteSource(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("source not found"))
			return
		}
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

type deleteSourcesRequest struct {
	IDs []string `json:"ids"`
}

func (s *Server) deleteSources(w http.ResponseWriter, r *http.Request) {
	var request deleteSourcesRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.IDs) == 0 {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("ids is required"))
		return
	}
	runtime, _ := runtimeFromContext(r.Context())
	summary, err := runtime.Compiler.DeleteSources(r.Context(), request.IDs)
	if err != nil {
		if err == sql.ErrNoRows && summary.DeletedSources == 0 {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("source not found"))
			return
		}
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) deleteWikiPage(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	if err := runtime.Compiler.DeletePage(r.Context(), chi.URLParam(r, "key")); err != nil {
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("wiki page not found"))
			return
		}
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) relinkWiki(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	added, err := runtime.Compiler.Relink(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"links_added": added})
}

func (s *Server) listCompilations(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runtime, _ := runtimeFromContext(r.Context())
	runs, err := runtime.Compiler.ListRuns(r.Context(), limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"compilations": runs})
}

func (s *Server) getCompilation(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	run, err := runtime.Compiler.GetRun(r.Context(), chi.URLParam(r, "id"))
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
	runtime, _ := runtimeFromContext(r.Context())
	if err := runtime.Compiler.RetryRender(r.Context(), chi.URLParam(r, "id")); err != nil {
		var pending *compiler.RenderPendingError
		if errors.As(err, &pending) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"status": compiler.RunRenderPending,
				"error":  errorEnvelope(w, http.StatusUnprocessableEntity, err),
			})
			return
		}
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("compilation run not found"))
			return
		}
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	run, err := runtime.Compiler.GetRun(r.Context(), chi.URLParam(r, "id"))
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
	runtime, _ := runtimeFromContext(r.Context())
	result, err := runtime.Sources.Ingest(r.Context(), sources.IngestInput{OriginalName: header.Filename, MediaType: header.Header.Get("Content-Type"), Data: data})
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
	runtime, _ := runtimeFromContext(r.Context())
	result, err := runtime.Sources.List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": result})
}
func (s *Server) getSource(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	item, err := runtime.Sources.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}
func (s *Server) getSourceChunks(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	result, err := runtime.Sources.Chunks(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chunks": result})
}
func (s *Server) getSourceChunk(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	result, err := runtime.Sources.Chunk(r.Context(), chi.URLParam(r, "id"))
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
	runtime, _ := runtimeFromContext(r.Context())
	result, err := runtime.Wiki.SourceTrace(r.Context(), chi.URLParam(r, "id"))
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
func (s *Server) getWikiGraph(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	graph, err := runtime.Wiki.Graph(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, graph)
}
func (s *Server) listWikiPages(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	result, err := runtime.Wiki.ListPages(r.Context(), r.URL.Query().Get("type"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pages": result})
}
func (s *Server) getWikiPage(w http.ResponseWriter, r *http.Request) {
	runtime, _ := runtimeFromContext(r.Context())
	result, err := runtime.Wiki.GetPage(r.Context(), chi.URLParam(r, "key"))
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
	runtime, _ := runtimeFromContext(r.Context())
	result, err := runtime.Wiki.Revisions(r.Context(), chi.URLParam(r, "key"))
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
	runtime, _ := runtimeFromContext(r.Context())
	result, err := runtime.Wiki.Diff(r.Context(), chi.URLParam(r, "key"), revision)
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
	runtime, _ := runtimeFromContext(r.Context())
	issues, err := runtime.Wiki.Lint(r.Context())
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
	writeJSON(w, status, map[string]any{"error": errorEnvelope(w, status, err)})
}

func errorEnvelope(w http.ResponseWriter, status int, err error) map[string]string {
	id := requestIDFromRequest(w)
	code := errorCode(status)
	message := logging.SafeSummary(err)
	logging.Logger(context.Background()).Error("http handler failed", "status", status, "code", code, "error", logging.SafeDetail(err), "request_id", id)
	if status >= http.StatusInternalServerError {
		message = "internal server error; see server logs"
	}
	return map[string]string{"code": code, "message": message, "request_id": id}
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
	Language  string                 `json:"language"`
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
	runtime, _ := runtimeFromContext(r.Context())
	llmBaseURL, llmModel := strings.TrimSpace(input.LLM.Endpoint), strings.TrimSpace(input.LLM.Model)
	embeddingBaseURL, embeddingModel := strings.TrimSpace(input.Embedding.Endpoint), strings.TrimSpace(input.Embedding.Model)
	apiKey := input.LLM.APIKey
	if apiKey == "" {
		apiKey = runtime.Effective.LLMAPIKey
	}
	language := input.Language
	if language == "" {
		language = runtime.KnowledgeBase.Language
	}
	updated, err := s.knowledgeBaseService().Update(r.Context(), runtime.KnowledgeBase.ID, knowledgebase.UpdateInput{
		Name: runtime.KnowledgeBase.Name, Description: runtime.KnowledgeBase.Description, Language: language,
		LLMBaseURL: llmBaseURL, LLMAPIKey: apiKey, LLMModel: llmModel,
		EmbeddingBaseURL: embeddingBaseURL, EmbeddingModel: embeddingModel,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	// Keep the legacy default workspace graph hot-updated for callers that
	// construct services directly. Non-default workspaces stay request-scoped.
	if runtime.KnowledgeBase.ID == knowledgebase.DefaultID {
		s.mu.Lock()
		next := s.Config
		next.CompilerEndpoint, next.CompilerLLMKey, next.CompilerModel = llmBaseURL, apiKey, llmModel
		next.ChatEndpoint, next.ChatLLMKey, next.ChatModel = llmBaseURL, apiKey, llmModel
		next.EmbeddingEndpoint, next.EmbeddingModel = embeddingBaseURL, embeddingModel
		next.EmbeddingBatchSize = input.Embedding.BatchSize
		s.applyProviderConfigLocked(next)
		s.mu.Unlock()
	}
	embeddingChanged := runtime.Effective.EmbeddingBaseURL != embeddingBaseURL || runtime.Effective.EmbeddingModel != embeddingModel
	if embeddingChanged {
		if _, err := s.DB.ExecContext(r.Context(), `UPDATE wiki_pages SET index_status = 'index_pending' WHERE status = 'active' AND knowledge_base_id = ?`, runtime.KnowledgeBase.ID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
	}
	nextRuntime, err := s.buildRuntime(r.Context(), updated.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	settings, providers := publicEffectiveSettings(nextRuntime), providerStatus(nextRuntime.Effective)
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings, "providers": providers, "reindex_required": embeddingChanged})
}

func (s *Server) applyProviderConfig(next config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyProviderConfigLocked(next)
}

func (s *Server) applyProviderConfigLocked(next config.Config) {
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
		Embedding: PublicEmbeddingSettings{Endpoint: c.EmbeddingEndpoint, Model: c.EmbeddingModel, BatchSize: c.EmbeddingBatchSize}, Language: knowledgebase.LanguageZH,
	}
}

func publicEffectiveSettings(runtime requestRuntime) PublicSettings {
	return PublicSettings{
		LLM:       PublicLLMSettings{Endpoint: runtime.Effective.LLMBaseURL, Model: runtime.Effective.LLMModel, APIKeySet: runtime.Effective.LLMAPIKey != ""},
		Embedding: PublicEmbeddingSettings{Endpoint: runtime.Effective.EmbeddingBaseURL, Model: runtime.Effective.EmbeddingModel, BatchSize: runtime.Indexer.BatchSize},
		Language:  runtime.Effective.Language,
	}
}

func providerStatus(value knowledgebase.EffectiveConfig) config.ProviderStatus {
	llmConfigured := value.LLMBaseURL != "" && value.LLMAPIKey != "" && value.LLMModel != ""
	return config.ProviderStatus{
		CompilerLLM: config.Provider{Configured: llmConfigured, Endpoint: value.LLMBaseURL != "", Model: value.LLMModel},
		ChatLLM:     config.Provider{Configured: llmConfigured, Endpoint: value.LLMBaseURL != "", Model: value.LLMModel},
		Embedding:   config.Provider{Configured: value.EmbeddingBaseURL != "" && value.EmbeddingModel != "", Endpoint: value.EmbeddingBaseURL != "", Model: value.EmbeddingModel},
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
		next.ServeHTTP(w, r.WithContext(logging.WithRequestID(r.Context(), id)))
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (w *responseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *responseRecorder) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		logging.Logger(r.Context()).Info("http request", "method", r.Method, "path", r.URL.Path, "status", status, "duration_ms", logging.Duration(started))
	})
}

func requestIDFromRequest(w http.ResponseWriter) string { return w.Header().Get("X-Request-ID") }

func errorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusUnprocessableEntity:
		return "unprocessable_entity"
	case http.StatusInternalServerError:
		return "internal_error"
	default:
		return fmt.Sprintf("http_%d", status)
	}
}
