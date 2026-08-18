package main

import (
	"context"
	"github.com/joeychen/llm-wiki-demo/backend/internal/api"
	"github.com/joeychen/llm-wiki-demo/backend/internal/compiler"
	"github.com/joeychen/llm-wiki-demo/backend/internal/config"
	"github.com/joeychen/llm-wiki-demo/backend/internal/db"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	"github.com/joeychen/llm-wiki-demo/backend/internal/sources"
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
	slog.Info("server started", "addr", cfg.Addr, "providers", cfg.ProviderStatus())
	server := &api.Server{DB: database, Config: cfg, Sources: sources.Service{DB: database, DataRoot: cfg.DataRoot}, Compiler: compiler.Service{DB: database, DataRoot: cfg.DataRoot, LLM: compilerLLM}}
	if err = http.ListenAndServe(cfg.Addr, server.Router()); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
