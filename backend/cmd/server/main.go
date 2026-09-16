package main

import (
	"context"
	"github.com/joeychen/llm-wiki-demo/backend/internal/api"
	chatservice "github.com/joeychen/llm-wiki-demo/backend/internal/chat"
	"github.com/joeychen/llm-wiki-demo/backend/internal/compiler"
	"github.com/joeychen/llm-wiki-demo/backend/internal/config"
	"github.com/joeychen/llm-wiki-demo/backend/internal/db"
	"github.com/joeychen/llm-wiki-demo/backend/internal/indexing"
	"github.com/joeychen/llm-wiki-demo/backend/internal/knowledgebase"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	"github.com/joeychen/llm-wiki-demo/backend/internal/logging"
	queryservice "github.com/joeychen/llm-wiki-demo/backend/internal/query"
	"github.com/joeychen/llm-wiki-demo/backend/internal/retrieval"
	"github.com/joeychen/llm-wiki-demo/backend/internal/sources"
	"github.com/joeychen/llm-wiki-demo/backend/internal/wiki"
	"net/http"
	"os"
)

func main() {
	cfg := config.Load()
	closeLog, err := logging.Init(cfg.LogLevel, cfg.LogFile)
	if err != nil {
		os.Stderr.WriteString("initialize logging: " + err.Error() + "\n")
		os.Exit(1)
	}
	defer closeLog()
	database, err := db.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		logging.Logger(context.Background()).Error("database startup failed", "error", logging.SafeSummary(err))
		os.Exit(1)
	}
	defer database.Close()
	if err := config.ApplyStored(context.Background(), database, &cfg); err != nil {
		logging.Logger(context.Background()).Error("load stored provider settings failed", "error", logging.SafeSummary(err))
		os.Exit(1)
	}
	var compilerLLM llm.LLMClient
	if cfg.CompilerLLMConfigured() {
		compilerLLM = llm.NewOpenAICompatible(cfg.CompilerEndpoint, cfg.CompilerLLMKey, cfg.CompilerModel, cfg.CompilerPromptDir)
	} else if cfg.CompilerFakeFallback {
		compilerLLM = llm.DeterministicFake{}
	}
	var chatLLM llm.GenerationClient
	if cfg.ChatLLMConfigured() {
		chatLLM = llm.NewOpenAICompatible(cfg.ChatEndpoint, cfg.ChatLLMKey, cfg.ChatModel, cfg.ChatPromptDir)
	} else if cfg.ChatFakeFallback {
		chatLLM = llm.DeterministicFake{}
	}
	embedder := llm.EmbeddingClient(llm.NewOllamaEmbedding(cfg.EmbeddingEndpoint, cfg.EmbeddingModel))
	indexer := indexing.Service{DB: database, Embedder: embedder, Model: cfg.EmbeddingModel, ProviderID: indexing.ProviderIdentity(cfg.EmbeddingEndpoint, cfg.EmbeddingModel), BatchSize: cfg.EmbeddingBatchSize, IndexDir: cfg.IndexDir}
	logging.Logger(context.Background()).Info("server started", "addr", cfg.Addr, "providers", cfg.ProviderStatus())
	sharedRetriever := retrieval.Service{DB: database, Embedder: embedder, IndexDir: cfg.IndexDir}
	server := &api.Server{DB: database, Config: cfg, KnowledgeBases: knowledgebase.Service{DB: database}, Sources: sources.Service{DB: database, DataRoot: cfg.DataRoot}, Compiler: compiler.Service{DB: database, DataRoot: cfg.DataRoot, LLM: compilerLLM}, Wiki: wiki.Service{DB: database, DataRoot: cfg.DataRoot}, Indexer: indexer, Retriever: sharedRetriever, Query: queryservice.Service{Generator: chatLLM}, Chat: chatservice.Service{DB: database, Generator: chatLLM}}
	server.BindRuntimeServices()
	if err = http.ListenAndServe(cfg.Addr, server.Router()); err != nil {
		logging.Logger(context.Background()).Error("server stopped", "error", logging.SafeSummary(err))
		os.Exit(1)
	}
}
