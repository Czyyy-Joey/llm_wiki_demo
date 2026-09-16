package knowledgebase

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultID  = "default"
	LanguageZH = "zh"
	LanguageEN = "en"
)

type KnowledgeBase struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	Status           string    `json:"status"`
	Language         string    `json:"language"`
	LLMBaseURL       string    `json:"llm_base_url,omitempty"`
	LLMModel         string    `json:"llm_model,omitempty"`
	LLMAPIKeySet     bool      `json:"llm_api_key_set"`
	EmbeddingBaseURL string    `json:"embedding_base_url,omitempty"`
	EmbeddingModel   string    `json:"embedding_model,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type Service struct{ DB *sql.DB }

type contextKey struct{}

func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, NormalizeID(id))
}

func IDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(contextKey{}).(string); ok {
		return NormalizeID(id)
	}
	return DefaultID
}

func Scope(ctx context.Context, configured string) string {
	if strings.TrimSpace(configured) != "" {
		return NormalizeID(configured)
	}
	return IDFromContext(ctx)
}

func NormalizeID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return DefaultID
	}
	return id
}

func NormalizeLanguage(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), LanguageEN) {
		return LanguageEN
	}
	return LanguageZH
}

func NewID(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "knowledge-base"
	}
	sum := sha256.Sum256([]byte(name + time.Now().UTC().Format(time.RFC3339Nano)))
	return "kb_" + hex.EncodeToString(sum[:8])
}

func (s Service) List(ctx context.Context) ([]KnowledgeBase, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, description, status, language, COALESCE(llm_base_url,''), COALESCE(llm_model,''), llm_api_key IS NOT NULL AND llm_api_key != '', COALESCE(embedding_base_url,''), COALESCE(embedding_model,''), created_at, updated_at FROM knowledge_bases WHERE status = 'active' ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []KnowledgeBase{}
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s Service) Get(ctx context.Context, id string) (KnowledgeBase, error) {
	return scan(s.DB.QueryRowContext(ctx, `SELECT id, name, description, status, language, COALESCE(llm_base_url,''), COALESCE(llm_model,''), llm_api_key IS NOT NULL AND llm_api_key != '', COALESCE(embedding_base_url,''), COALESCE(embedding_model,''), created_at, updated_at FROM knowledge_bases WHERE id = ?`, NormalizeID(id)))
}

type EffectiveConfig struct {
	Language, LLMBaseURL, LLMAPIKey, LLMModel, EmbeddingBaseURL, EmbeddingModel string
}

func (s Service) Effective(ctx context.Context, id string, defaults EffectiveConfig) (EffectiveConfig, error) {
	id = NormalizeID(id)
	var language, llmBaseURL, llmAPIKey, llmModel, embeddingBaseURL, embeddingModel sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT language, llm_base_url, llm_api_key, llm_model, embedding_base_url, embedding_model FROM knowledge_bases WHERE id = ? AND status = 'active'`, id).
		Scan(&language, &llmBaseURL, &llmAPIKey, &llmModel, &embeddingBaseURL, &embeddingModel)
	if err != nil {
		return EffectiveConfig{}, err
	}
	if language.String != "" {
		defaults.Language = NormalizeLanguage(language.String)
	}
	apply := func(value sql.NullString, target *string) {
		if value.Valid && strings.TrimSpace(value.String) != "" {
			*target = strings.TrimSpace(value.String)
		}
	}
	apply(llmBaseURL, &defaults.LLMBaseURL)
	apply(llmAPIKey, &defaults.LLMAPIKey)
	apply(llmModel, &defaults.LLMModel)
	apply(embeddingBaseURL, &defaults.EmbeddingBaseURL)
	apply(embeddingModel, &defaults.EmbeddingModel)
	return defaults, nil
}

func (s Service) Create(ctx context.Context, name, description, language string) (KnowledgeBase, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return KnowledgeBase{}, fmt.Errorf("knowledge base name is required")
	}
	now := time.Now().UTC()
	item := KnowledgeBase{ID: NewID(name), Name: name, Description: strings.TrimSpace(description), Status: "active", Language: NormalizeLanguage(language), CreatedAt: now, UpdatedAt: now}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO knowledge_bases (id, name, description, status, language, created_at, updated_at) VALUES (?, ?, ?, 'active', ?, ?, ?)`, item.ID, item.Name, item.Description, item.Language, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	return item, err
}

type UpdateInput struct{ Name, Description, Language, LLMBaseURL, LLMAPIKey, LLMModel, EmbeddingBaseURL, EmbeddingModel string }

func (s Service) Update(ctx context.Context, id string, input UpdateInput) (KnowledgeBase, error) {
	id = NormalizeID(id)
	old, err := s.Get(ctx, id)
	if err != nil {
		return KnowledgeBase{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = old.Name
	}
	language := NormalizeLanguage(input.Language)
	if input.Language == "" {
		language = old.Language
	}
	llmKey := input.LLMAPIKey
	if llmKey == "" && old.LLMAPIKeySet {
		var current string
		_ = s.DB.QueryRowContext(ctx, `SELECT COALESCE(llm_api_key,'') FROM knowledge_bases WHERE id = ?`, id).Scan(&current)
		llmKey = current
	}
	now := time.Now().UTC()
	result, err := s.DB.ExecContext(ctx, `UPDATE knowledge_bases SET name=?, description=?, language=?, llm_base_url=?, llm_api_key=?, llm_model=?, embedding_base_url=?, embedding_model=?, updated_at=? WHERE id=?`, name, strings.TrimSpace(input.Description), language, nullable(input.LLMBaseURL), nullable(llmKey), nullable(input.LLMModel), nullable(input.EmbeddingBaseURL), nullable(input.EmbeddingModel), now.Format(time.RFC3339Nano), id)
	if err != nil {
		return KnowledgeBase{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return KnowledgeBase{}, sql.ErrNoRows
	}
	return s.Get(ctx, id)
}

func (s Service) Archive(ctx context.Context, id string) error {
	id = NormalizeID(id)
	if id == DefaultID {
		return fmt.Errorf("default knowledge base cannot be archived")
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE knowledge_bases SET status='archived', updated_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

type scanner interface{ Scan(...any) error }

func scan(row scanner) (KnowledgeBase, error) {
	var item KnowledgeBase
	var created, updated string
	err := row.Scan(&item.ID, &item.Name, &item.Description, &item.Status, &item.Language, &item.LLMBaseURL, &item.LLMModel, &item.LLMAPIKeySet, &item.EmbeddingBaseURL, &item.EmbeddingModel, &created, &updated)
	if err != nil {
		return item, err
	}
	item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return item, nil
}
