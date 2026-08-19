package config

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Addr                 string
	DatabaseURL          string
	DataRoot             string
	CompilerLLMKey       string
	CompilerModel        string
	EmbeddingModel       string
	EmbeddingBatchSize   int
	CompilerEndpoint     string
	CompilerPromptDir    string
	CompilerFakeFallback bool
	ChatLLMKey           string
	ChatModel            string
	ChatEndpoint         string
	ChatPromptDir        string
	ChatFakeFallback     bool
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
	ChatLLM     Provider `json:"chat_llm"`
	Embedding   Provider `json:"embedding"`
}

func Load() Config {
	llmBaseURL := env("LLM_BASE_URL", "https://oneapi-comate.baidu-int.com/v1")
	llmAPIKey := os.Getenv("ONEAPI_API_KEY")
	llmModel := os.Getenv("LLM_MODEL")
	llmFakeFallback := envBool("LLM_FAKE_FALLBACK", false)
	return Config{
		Addr:                 env("APP_ADDR", "127.0.0.1:8080"),
		DatabaseURL:          env("DATABASE_URL", "file:../data/app.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"),
		DataRoot:             env("DATA_ROOT", "../data"),
		CompilerLLMKey:       env("COMPILER_LLM_API_KEY", llmAPIKey),
		CompilerModel:        env("COMPILER_LLM_MODEL", llmModel),
		EmbeddingModel:       env("EMBEDDING_MODEL", "qwen3-embedding:0.6b"),
		EmbeddingBatchSize:   envInt("EMBEDDING_BATCH_SIZE", 32),
		CompilerEndpoint:     env("COMPILER_LLM_BASE_URL", env("COMPILER_LLM_ENDPOINT", llmBaseURL)),
		CompilerPromptDir:    env("COMPILER_PROMPT_DIR", "prompts"),
		CompilerFakeFallback: envBool("COMPILER_LLM_FAKE_FALLBACK", llmFakeFallback),
		ChatLLMKey:           env("CHAT_LLM_API_KEY", llmAPIKey),
		ChatModel:            env("CHAT_LLM_MODEL", llmModel),
		ChatEndpoint:         env("CHAT_LLM_BASE_URL", env("CHAT_LLM_ENDPOINT", llmBaseURL)),
		ChatPromptDir:        env("CHAT_PROMPT_DIR", "prompts"),
		ChatFakeFallback:     envBool("CHAT_LLM_FAKE_FALLBACK", llmFakeFallback),
		EmbeddingEndpoint:    env("OLLAMA_BASE_URL", env("EMBEDDING_ENDPOINT", "http://127.0.0.1:11434")),
		IndexDir:             env("INDEX_DIR", "../data/indexes"),
	}
}

var storedKeys = map[string]func(*Config, string){
	"llm_base_url": func(c *Config, v string) {
		c.CompilerEndpoint, c.ChatEndpoint = v, v
	},
	"llm_api_key": func(c *Config, v string) {
		c.CompilerLLMKey, c.ChatLLMKey = v, v
	},
	"llm_model": func(c *Config, v string) {
		c.CompilerModel, c.ChatModel = v, v
	},
	"llm_fake_fallback": func(c *Config, v string) {
		c.CompilerFakeFallback, _ = strconv.ParseBool(v)
		c.ChatFakeFallback = c.CompilerFakeFallback
	},
	"compiler_endpoint":      func(c *Config, v string) { c.CompilerEndpoint = v },
	"compiler_base_url":      func(c *Config, v string) { c.CompilerEndpoint = v },
	"compiler_api_key":       func(c *Config, v string) { c.CompilerLLMKey = v },
	"compiler_model":         func(c *Config, v string) { c.CompilerModel = v },
	"compiler_fake_fallback": func(c *Config, v string) { c.CompilerFakeFallback, _ = strconv.ParseBool(v) },
	"chat_endpoint":          func(c *Config, v string) { c.ChatEndpoint = v },
	"chat_base_url":          func(c *Config, v string) { c.ChatEndpoint = v },
	"chat_api_key":           func(c *Config, v string) { c.ChatLLMKey = v },
	"chat_model":             func(c *Config, v string) { c.ChatModel = v },
	"chat_fake_fallback":     func(c *Config, v string) { c.ChatFakeFallback, _ = strconv.ParseBool(v) },
	"embedding_endpoint":     func(c *Config, v string) { c.EmbeddingEndpoint = v },
	"ollama_base_url":        func(c *Config, v string) { c.EmbeddingEndpoint = v },
	"embedding_model":        func(c *Config, v string) { c.EmbeddingModel = v },
	"embedding_batch_size":   func(c *Config, v string) { c.EmbeddingBatchSize, _ = strconv.Atoi(v) },
}

func ApplyStored(ctx context.Context, db *sql.DB, cfg *Config) error {
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM app_settings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for key, value := range values {
		if key == "llm_base_url" || key == "llm_api_key" || key == "llm_model" || key == "llm_fake_fallback" {
			continue
		}
		if apply, ok := storedKeys[key]; ok {
			apply(cfg, value)
		}
	}
	for _, key := range []string{"llm_base_url", "llm_api_key", "llm_model", "llm_fake_fallback"} {
		if value, ok := values[key]; ok {
			storedKeys[key](cfg, value)
		}
	}
	return nil
}

func SaveStored(ctx context.Context, db *sql.DB, values map[string]string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for key, value := range values {
		if _, ok := storedKeys[key]; !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings (key, value, updated_at) VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (c Config) CompilerLLMConfigured() bool {
	return c.CompilerLLMKey != "" && c.CompilerEndpoint != "" && c.CompilerModel != ""
}

func (c Config) ChatLLMConfigured() bool {
	return c.ChatLLMKey != "" && c.ChatEndpoint != "" && c.ChatModel != ""
}

func (c Config) ProviderStatus() ProviderStatus {
	return ProviderStatus{
		CompilerLLM: Provider{Configured: c.CompilerLLMKey != "" && c.CompilerEndpoint != "" && c.CompilerModel != "", Endpoint: c.CompilerEndpoint != "", Model: c.CompilerModel},
		ChatLLM:     Provider{Configured: c.ChatLLMConfigured(), Endpoint: c.ChatEndpoint != "", Model: c.ChatModel},
		Embedding:   Provider{Configured: c.EmbeddingEndpoint != "" && c.EmbeddingModel != "", Endpoint: c.EmbeddingEndpoint != "", Model: c.EmbeddingModel},
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
