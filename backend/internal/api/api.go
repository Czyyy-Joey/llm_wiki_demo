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
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/joeychen/llm-wiki-demo/backend/internal/compiler"
	"github.com/joeychen/llm-wiki-demo/backend/internal/config"
	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/indexing"
	"github.com/joeychen/llm-wiki-demo/backend/internal/retrieval"
	"github.com/joeychen/llm-wiki-demo/backend/internal/sources"
	"github.com/joeychen/llm-wiki-demo/backend/internal/wiki"
)

type Server struct {
	DB        *sql.DB
	Config    config.Config
	Sources   sources.Service
	Compiler  compiler.Service
	Wiki      wiki.Service
	Indexer   indexing.Service
	Retriever retrieval.Service
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
	}
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
		out.Body.Providers = s.Config.ProviderStatus()
		return out, nil
	})
	r.Post("/api/sources", s.uploadSource)
	r.Get("/api/sources", s.listSources)
	r.Get("/api/sources/{id}", s.getSource)
	r.Get("/api/sources/{id}/chunks", s.getSourceChunks)
	r.Get("/api/sources/{id}/wiki", s.getSourceWiki)
	r.Get("/api/wiki/pages", s.listWikiPages)
	r.Get("/api/wiki/pages/{key}", s.getWikiPage)
	r.Get("/api/wiki/pages/{key}/revisions", s.getWikiRevisions)
	r.Get("/api/wiki/pages/{key}/revisions/{revision}", s.getWikiRevisionDiff)
	r.Get("/api/wiki/lint", s.lintWiki)
	r.Post("/api/compilations", s.createCompilation)
	r.Get("/api/compilations/{id}", s.getCompilation)
	r.Post("/api/compilations/{id}/render", s.retryCompilationRender)
	r.Post("/api/indexes/reindex", s.reindex)
	r.Get("/api/retrieval/search", s.search)
	r.Get("/api/retrieval/traces/{id}", s.getRetrievalTrace)
	return requestID(r)
}

func (s *Server) reindex(w http.ResponseWriter, r *http.Request) {
	result, err := s.Indexer.Reindex(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	result, err := s.Retriever.Search(r.Context(), r.URL.Query().Get("q"))
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

type compilationRequest struct {
	DocumentID string `json:"document_id"`
}

func (s *Server) createCompilation(w http.ResponseWriter, r *http.Request) {
	var request compilationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.DocumentID == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("document_id is required"))
		return
	}
	result, err := s.Compiler.Compile(r.Context(), request.DocumentID)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, result)
		return
	}
	writeJSON(w, http.StatusCreated, result)
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
		domain.Conversation{}, domain.Message{},
	} {
		registry.Schema(reflect.TypeOf(contract), true, "")
	}
}

func (s *Server) health() (*HealthResponse, error) {
	out := &HealthResponse{}
	out.Body.Status = "ok"
	out.Body.Database = "ok"
	out.Body.Providers = s.Config.ProviderStatus()
	if err := s.DB.Ping(); err != nil {
		out.Body.Status = "degraded"
		out.Body.Database = "error"
	}
	return out, nil
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
