package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"

	chatservice "github.com/joeychen/llm-wiki-demo/backend/internal/chat"
	"github.com/joeychen/llm-wiki-demo/backend/internal/compiler"
	"github.com/joeychen/llm-wiki-demo/backend/internal/indexing"
	"github.com/joeychen/llm-wiki-demo/backend/internal/knowledgebase"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	queryservice "github.com/joeychen/llm-wiki-demo/backend/internal/query"
	"github.com/joeychen/llm-wiki-demo/backend/internal/retrieval"
	"github.com/joeychen/llm-wiki-demo/backend/internal/sources"
	"github.com/joeychen/llm-wiki-demo/backend/internal/wiki"
)

const knowledgeBaseHeader = "X-Knowledge-Base-ID"

type runtimeContextKey struct{}

type requestRuntime struct {
	KnowledgeBase knowledgebase.KnowledgeBase
	Effective     knowledgebase.EffectiveConfig
	Sources       sources.Service
	Compiler      compiler.Service
	Wiki          wiki.Service
	Indexer       indexing.Service
	Retriever     retrieval.Service
	Query         queryservice.Service
	Chat          chatservice.Service
}

func (s *Server) knowledgeBaseService() knowledgebase.Service {
	if s.KnowledgeBases.DB != nil {
		return s.KnowledgeBases
	}
	return knowledgebase.Service{DB: s.DB}
}

func (s *Server) buildRuntime(ctx context.Context, id string) (requestRuntime, error) {
	id = knowledgebase.NormalizeID(id)
	kbService := s.knowledgeBaseService()
	kb, err := kbService.Get(ctx, id)
	if err != nil {
		return requestRuntime{}, err
	}
	if kb.Status != "active" {
		return requestRuntime{}, fmt.Errorf("knowledge base %s is not active", id)
	}

	s.mu.RLock()
	cfg := s.Config
	s.mu.RUnlock()
	effective, err := kbService.Effective(ctx, id, knowledgebase.EffectiveConfig{
		Language:         kb.Language,
		LLMBaseURL:       cfg.CompilerEndpoint,
		LLMAPIKey:        cfg.CompilerLLMKey,
		LLMModel:         cfg.CompilerModel,
		EmbeddingBaseURL: cfg.EmbeddingEndpoint,
		EmbeddingModel:   cfg.EmbeddingModel,
	})
	if err != nil {
		return requestRuntime{}, err
	}

	var compilerLLM llm.LLMClient
	var generator llm.GenerationClient
	if effective.LLMBaseURL != "" && effective.LLMAPIKey != "" && effective.LLMModel != "" {
		client := llm.NewOpenAICompatible(effective.LLMBaseURL, effective.LLMAPIKey, effective.LLMModel, cfg.CompilerPromptDir)
		compilerLLM, generator = client, client
	} else if cfg.CompilerFakeFallback || cfg.ChatFakeFallback {
		fake := llm.DeterministicFake{}
		compilerLLM, generator = fake, fake
	}
	if id == knowledgebase.DefaultID {
		if compilerLLM == nil && s.Compiler.LLM != nil {
			compilerLLM = s.Compiler.LLM
		}
		if generator == nil {
			if s.Query.Generator != nil {
				generator = s.Query.Generator
			} else if s.Chat.Generator != nil {
				generator = s.Chat.Generator
			}
		}
	}
	embedder := llm.EmbeddingClient(llm.NewOllamaEmbedding(effective.EmbeddingBaseURL, effective.EmbeddingModel))
	indexRoot := cfg.IndexDir
	dataRoot := cfg.DataRoot
	if id == knowledgebase.DefaultID {
		if s.Indexer.Embedder != nil {
			embedder = s.Indexer.Embedder
		} else if s.Retriever.Embedder != nil {
			embedder = s.Retriever.Embedder
		}
		if s.Indexer.IndexDir != "" {
			indexRoot = s.Indexer.IndexDir
		}
		if s.Sources.DataRoot != "" {
			dataRoot = s.Sources.DataRoot
		} else if s.Compiler.DataRoot != "" {
			dataRoot = s.Compiler.DataRoot
		}
	}
	indexDir := scopedIndexDir(indexRoot, id)
	projectionRoot := scopedProjectionRoot(dataRoot, id)
	indexer := indexing.Service{
		DB: s.DB, Embedder: embedder, Model: effective.EmbeddingModel,
		ProviderID: indexing.ProviderIdentity(effective.EmbeddingBaseURL, effective.EmbeddingModel),
		BatchSize:  cfg.EmbeddingBatchSize, IndexDir: indexDir, KnowledgeBaseID: id,
	}
	retriever := retrieval.Service{DB: s.DB, Embedder: embedder, IndexDir: indexDir, KnowledgeBaseID: id}
	query := queryservice.Service{Retriever: retriever, Generator: generator, Language: effective.Language}
	// Keep an explicitly injected custom retriever available for embedded
	// callers and contract tests. The normal server graph is a retrieval.Service
	// and is rebuilt here so provider hot updates cannot leave it stale.
	if id == knowledgebase.DefaultID {
		if _, normal := s.Query.Retriever.(retrieval.Service); !normal && s.Query.Retriever != nil {
			query.Retriever = s.Query.Retriever
		}
	}
	chat := chatservice.Service{DB: s.DB, Query: query, Generator: generator, KnowledgeBaseID: id, Language: effective.Language}
	compilerService := compiler.Service{DB: s.DB, DataRoot: projectionRoot, LLM: compilerLLM, Index: &indexer, KnowledgeBaseID: id, Language: effective.Language}
	return requestRuntime{
		KnowledgeBase: kb,
		Effective:     effective,
		Sources:       sources.Service{DB: s.DB, DataRoot: dataRoot, KnowledgeBaseID: id},
		Compiler:      compilerService,
		Wiki:          wiki.Service{DB: s.DB, DataRoot: projectionRoot, KnowledgeBaseID: id},
		Indexer:       indexer, Retriever: retriever, Query: query, Chat: chat,
	}, nil
}

func scopedProjectionRoot(root, id string) string {
	if knowledgebase.NormalizeID(id) == knowledgebase.DefaultID {
		return root
	}
	return filepath.Join(root, "knowledge-bases", id)
}

func scopedIndexDir(root, id string) string {
	if knowledgebase.NormalizeID(id) == knowledgebase.DefaultID {
		return root
	}
	return filepath.Join(root, id)
}

func (s *Server) knowledgeBaseScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := knowledgebase.NormalizeID(r.Header.Get(knowledgeBaseHeader))
		runtime, err := s.buildRuntime(r.Context(), id)
		if err != nil {
			status := http.StatusBadRequest
			if err == sql.ErrNoRows {
				status = http.StatusNotFound
			}
			writeJSONError(w, status, fmt.Errorf("select knowledge base %q: %w", id, err))
			return
		}
		w.Header().Set(knowledgeBaseHeader, id)
		ctx := knowledgebase.WithID(r.Context(), id)
		ctx = context.WithValue(ctx, runtimeContextKey{}, runtime)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func runtimeFromContext(ctx context.Context) (requestRuntime, bool) {
	runtime, ok := ctx.Value(runtimeContextKey{}).(requestRuntime)
	return runtime, ok
}
