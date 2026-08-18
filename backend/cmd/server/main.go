package main

import (
	"context"
	"github.com/joeychen/llm-wiki-demo/backend/internal/api"
	"github.com/joeychen/llm-wiki-demo/backend/internal/compiler"
	"github.com/joeychen/llm-wiki-demo/backend/internal/config"
	"github.com/joeychen/llm-wiki-demo/backend/internal/db"
	"github.com/joeychen/llm-wiki-demo/backend/internal/indexing"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	"github.com/joeychen/llm-wiki-demo/backend/internal/retrieval"
	"github.com/joeychen/llm-wiki-demo/backend/internal/sources"
	"github.com/joeychen/llm-wiki-demo/backend/internal/wiki"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	cfg := config.Load()
	database, err := db.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		slog.Error("database startup failed", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	var compilerLLM llm.LLMClient
	if cfg.CompilerLLMConfigured() {
		compilerLLM = llm.NewOpenAICompatible(cfg.CompilerEndpoint, cfg.CompilerLLMKey, cfg.CompilerModel, cfg.CompilerPromptDir)
	} else if cfg.CompilerFakeFallback {
		compilerLLM = llm.DeterministicFake{}
	}
	embedder := llm.EmbeddingClient(llm.DeterministicEmbedding{})
	if cfg.EmbeddingAPIKey != "" && cfg.EmbeddingEndpoint != "" && cfg.EmbeddingModel != "" {
		embedder = llm.NewOpenAIEmbedding(cfg.EmbeddingEndpoint, cfg.EmbeddingAPIKey, cfg.EmbeddingModel)
	}
	indexer := indexing.Service{DB: database, Embedder: embedder, Model: cfg.EmbeddingModel, BatchSize: cfg.EmbeddingBatchSize, IndexDir: cfg.IndexDir}
	slog.Info("server started", "addr", cfg.Addr, "providers", cfg.ProviderStatus())
	server := &api.Server{DB: database, Config: cfg, Sources: sources.Service{DB: database, DataRoot: cfg.DataRoot}, Compiler: compiler.Service{DB: database, DataRoot: cfg.DataRoot, LLM: compilerLLM, Index: &indexer}, Wiki: wiki.Service{DB: database, DataRoot: cfg.DataRoot}, Indexer: indexer, Retriever: retrieval.Service{DB: database, Embedder: embedder, IndexDir: cfg.IndexDir}}
	if err = http.ListenAndServe(cfg.Addr, server.Router()); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
