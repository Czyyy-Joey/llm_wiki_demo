package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/joeychen/llm-wiki-demo/backend/internal/knowledgebase"
)

type knowledgeBaseInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Language    string `json:"language"`
}

func (s *Server) listKnowledgeBases(w http.ResponseWriter, r *http.Request) {
	items, err := s.knowledgeBaseService().List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"knowledge_bases": items})
}

func (s *Server) createKnowledgeBase(w http.ResponseWriter, r *http.Request) {
	var input knowledgeBaseInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid knowledge base payload"))
		return
	}
	item, err := s.knowledgeBaseService().Create(r.Context(), input.Name, input.Description, input.Language)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) getKnowledgeBase(w http.ResponseWriter, r *http.Request) {
	item, err := s.knowledgeBaseService().Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if err == sql.ErrNoRows {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("knowledge base not found"))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) updateKnowledgeBase(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	current, err := s.knowledgeBaseService().Get(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("knowledge base not found"))
		return
	}
	var input knowledgeBaseInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid knowledge base payload"))
		return
	}
	item, err := s.knowledgeBaseService().Update(r.Context(), id, knowledgebase.UpdateInput{
		Name: input.Name, Description: input.Description, Language: input.Language,
		LLMBaseURL: current.LLMBaseURL, LLMModel: current.LLMModel,
		EmbeddingBaseURL: current.EmbeddingBaseURL, EmbeddingModel: current.EmbeddingModel,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) archiveKnowledgeBase(w http.ResponseWriter, r *http.Request) {
	if err := s.knowledgeBaseService().Archive(r.Context(), chi.URLParam(r, "id")); err != nil {
		status := http.StatusBadRequest
		if err == sql.ErrNoRows {
			status = http.StatusNotFound
		}
		writeJSONError(w, status, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
