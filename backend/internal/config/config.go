package config

import (
	"os"
	"strconv"
)

type Config struct {
	Addr                 string
	DatabaseURL          string
	DataRoot             string
	CompilerLLMKey       string
	CompilerModel        string
	EmbeddingAPIKey      string
	EmbeddingModel       string
	EmbeddingDimension   int
	EmbeddingBatchSize   int
	CompilerEndpoint     string
	CompilerPromptDir    string
	CompilerFakeFallback bool
	EmbeddingEndpoint    string
	IndexDir             string
}

type Provider struct {
	Configured bool   `json:"configured"`
	Endpoint   bool   `json:"endpoint_configured"`
	Model      string `json:"model,omitempty"`
}

type ProviderStatus struct {
	CompilerLLM Provider `json:"compiler_llm"`
	Embedding   Provider `json:"embedding"`
}

func Load() Config {
	return Config{
		Addr:                 env("APP_ADDR", "127.0.0.1:8080"),
		DatabaseURL:          env("DATABASE_URL", "file:../data/app.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"),
		DataRoot:             env("DATA_ROOT", "../data"),
		CompilerLLMKey:       os.Getenv("COMPILER_LLM_API_KEY"),
		CompilerModel:        os.Getenv("COMPILER_LLM_MODEL"),
		EmbeddingAPIKey:      os.Getenv("EMBEDDING_API_KEY"),
		EmbeddingModel:       os.Getenv("EMBEDDING_MODEL"),
		EmbeddingDimension:   envInt("EMBEDDING_DIMENSION", 1536),
		EmbeddingBatchSize:   envInt("EMBEDDING_BATCH_SIZE", 32),
		CompilerEndpoint:     os.Getenv("COMPILER_LLM_ENDPOINT"),
		CompilerPromptDir:    env("COMPILER_PROMPT_DIR", "prompts"),
		CompilerFakeFallback: envBool("COMPILER_LLM_FAKE_FALLBACK", false),
		EmbeddingEndpoint:    os.Getenv("EMBEDDING_ENDPOINT"),
		IndexDir:             env("INDEX_DIR", "../data/indexes"),
	}
}

func (c Config) CompilerLLMConfigured() bool {
	return c.CompilerLLMKey != "" && c.CompilerEndpoint != "" && c.CompilerModel != ""
}

func (c Config) ProviderStatus() ProviderStatus {
	return ProviderStatus{
		CompilerLLM: Provider{Configured: c.CompilerLLMKey != "" && c.CompilerEndpoint != "" && c.CompilerModel != "", Endpoint: c.CompilerEndpoint != "", Model: c.CompilerModel},
		Embedding:   Provider{Configured: c.EmbeddingAPIKey != "" && c.EmbeddingEndpoint != "" && c.EmbeddingModel != "" && c.EmbeddingDimension > 0, Endpoint: c.EmbeddingEndpoint != "", Model: c.EmbeddingModel},
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
