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

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/joeychen/llm-wiki-demo/backend/internal/compiler"
	"github.com/joeychen/llm-wiki-demo/backend/internal/config"
	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/sources"
)

type Server struct {
	DB       *sql.DB
	Config   config.Config
	Sources  sources.Service
	Compiler compiler.Service
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
	r.Post("/api/compilations", s.createCompilation)
	r.Get("/api/compilations/{id}", s.getCompilation)
	r.Post("/api/compilations/{id}/render", s.retryCompilationRender)
	return requestID(r)
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
