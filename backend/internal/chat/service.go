package chat

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/knowledgebase"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	"github.com/joeychen/llm-wiki-demo/backend/internal/logging"
	queryservice "github.com/joeychen/llm-wiki-demo/backend/internal/query"
	"github.com/joeychen/llm-wiki-demo/backend/internal/retrieval"
)

type Service struct {
	DB              *sql.DB
	Query           queryservice.Service
	Generator       llm.GenerationClient
	HistoryBudget   int
	KnowledgeBaseID string
	Language        string
}

func (s Service) scope(ctx context.Context) string {
	return knowledgebase.Scope(ctx, s.KnowledgeBaseID)
}

type Detail struct {
	Conversation domain.Conversation `json:"conversation"`
	Messages     []domain.Message    `json:"messages"`
}

type TurnResult struct {
	UserMessage      domain.Message      `json:"user_message"`
	AssistantMessage domain.Message      `json:"assistant_message"`
	Result           queryservice.Result `json:"result"`
}

type Retriever interface {
	Search(context.Context, string) (retrieval.Result, error)
}

func (s Service) Create(ctx context.Context, title string) (domain.Conversation, error) {
	if s.DB == nil {
		return domain.Conversation{}, fmt.Errorf("chat database is nil")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "New conversation"
	}
	now := time.Now().UTC()
	conversation := domain.Conversation{ID: stableID("conversation", s.scope(ctx)+title, now), Title: title, CreatedAt: now, UpdatedAt: now}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO conversations (id, knowledge_base_id, title, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, conversation.ID, s.scope(ctx), conversation.Title, formatTime(now), formatTime(now))
	return conversation, err
}

func (s Service) List(ctx context.Context) ([]domain.Conversation, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, title, created_at, updated_at FROM conversations WHERE knowledge_base_id = ? ORDER BY updated_at DESC, id`, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Conversation{}
	for rows.Next() {
		var item domain.Conversation
		var created, updated string
		if err := rows.Scan(&item.ID, &item.Title, &created, &updated); err != nil {
			return nil, err
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s Service) Get(ctx context.Context, id string) (Detail, error) {
	var detail Detail
	var created, updated string
	err := s.DB.QueryRowContext(ctx, `SELECT id, title, created_at, updated_at FROM conversations WHERE id = ? AND knowledge_base_id = ?`, id, s.scope(ctx)).Scan(&detail.Conversation.ID, &detail.Conversation.Title, &created, &updated)
	if err != nil {
		return Detail{}, err
	}
	detail.Conversation.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	detail.Conversation.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	rows, err := s.DB.QueryContext(ctx, `SELECT id, conversation_id, role, content, COALESCE(retrieval_trace_id, ''), COALESCE(standalone_query, ''), COALESCE(citations_json, '[]'), COALESCE(context_json, '[]'), created_at FROM messages WHERE conversation_id = ? AND knowledge_base_id = ? ORDER BY created_at, CASE role WHEN 'user' THEN 0 ELSE 1 END, id`, id, s.scope(ctx))
	if err != nil {
		return Detail{}, err
	}
	defer rows.Close()
	detail.Messages = []domain.Message{}
	for rows.Next() {
		var item domain.Message
		var citations, contextJSON, timestamp string
		if err := rows.Scan(&item.ID, &item.ConversationID, &item.Role, &item.Content, &item.RetrievalTraceID, &item.StandaloneQuery, &citations, &contextJSON, &timestamp); err != nil {
			return Detail{}, err
		}
		_ = json.Unmarshal([]byte(citations), &item.Citations)
		_ = json.Unmarshal([]byte(contextJSON), &item.Context)
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, timestamp)
		detail.Messages = append(detail.Messages, item)
	}
	return detail, rows.Err()
}

func (s Service) Delete(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM messages WHERE conversation_id = ? AND knowledge_base_id = ?`, id, s.scope(ctx)); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id = ? AND knowledge_base_id = ?`, id, s.scope(ctx))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s Service) Send(ctx context.Context, conversationID, question string) (TurnResult, error) {
	ctx = llm.WithLanguage(ctx, s.Language)
	started := time.Now()
	question = strings.TrimSpace(question)
	logging.Logger(ctx).Info("chat turn started", "conversation_id", conversationID, "question_length", utf8.RuneCountInString(question))
	if question == "" {
		return TurnResult{}, fmt.Errorf("question is required")
	}
	if s.Generator == nil {
		return TurnResult{}, fmt.Errorf("chat generation provider is not configured")
	}
	history, err := s.recentHistory(ctx, conversationID)
	if err != nil {
		return TurnResult{}, err
	}
	raw, err := s.Generator.Rewrite(ctx, question, history)
	if err != nil {
		logging.Logger(ctx).Error("chat query rewrite failed", "conversation_id", conversationID, "history_turns", len(history), "error", logging.SafeSummary(err))
		return TurnResult{}, err
	}
	rewritten, err := llm.DecodeStrict[domain.StandaloneQuery](raw)
	if err != nil {
		return TurnResult{}, err
	}
	rewritten.Query = strings.TrimSpace(rewritten.Query)
	if rewritten.Query == "" {
		return TurnResult{}, fmt.Errorf("standalone query is empty")
	}
	answer, err := s.Query.AskWithRetrievalQuery(ctx, question, rewritten.Query)
	if err != nil {
		logging.Logger(ctx).Error("chat answer failed", "conversation_id", conversationID, "error", logging.SafeSummary(err))
		return TurnResult{}, err
	}
	now := time.Now().UTC()
	user := domain.Message{ID: stableID("message-user", conversationID+question, now), ConversationID: conversationID, Role: "user", Content: question, CreatedAt: now}
	assistant := domain.Message{ID: stableID("message-assistant", conversationID+answer.Answer, now), ConversationID: conversationID, Role: "assistant", Content: answer.Answer, RetrievalTraceID: answer.Trace.ID, StandaloneQuery: rewritten.Query, Citations: answer.Citations, Context: answer.Context, CreatedAt: now}
	if err := s.saveTurn(ctx, user, assistant); err != nil {
		logging.Logger(ctx).Error("chat persistence failed", "conversation_id", conversationID, "trace_id", answer.Trace.ID, "error", logging.SafeSummary(err))
		return TurnResult{}, err
	}
	logging.Logger(ctx).Info("chat turn completed", "conversation_id", conversationID, "trace_id", answer.Trace.ID, "history_turns", len(history), "context_items", len(answer.Context), "citations", len(answer.Citations), "duration_ms", logging.Duration(started))
	return TurnResult{UserMessage: user, AssistantMessage: assistant, Result: answer}, nil
}

func (s Service) recentHistory(ctx context.Context, conversationID string) ([]llm.ChatTurn, error) {
	var exists int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE id = ? AND knowledge_base_id = ?`, conversationID, s.scope(ctx)).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, sql.ErrNoRows
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT role, content FROM messages WHERE conversation_id = ? AND knowledge_base_id = ? ORDER BY created_at DESC, CASE role WHEN 'assistant' THEN 1 ELSE 0 END DESC, id DESC`, conversationID, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	budget := s.HistoryBudget
	if budget <= 0 {
		budget = 1200
	}
	used := 0
	reversed := []llm.ChatTurn{}
	for rows.Next() {
		var item llm.ChatTurn
		if err := rows.Scan(&item.Role, &item.Content); err != nil {
			return nil, err
		}
		cost := historyCost(item.Content)
		if used+cost > budget {
			break
		}
		used += cost
		reversed = append(reversed, item)
	}
	result := make([]llm.ChatTurn, len(reversed))
	for i := range reversed {
		result[len(reversed)-1-i] = reversed[i]
	}
	return result, rows.Err()
}

func (s Service) saveTurn(ctx context.Context, user, assistant domain.Message) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages (id, knowledge_base_id, conversation_id, role, content, created_at) VALUES (?, ?, ?, ?, ?, ?)`, user.ID, s.scope(ctx), user.ConversationID, user.Role, user.Content, formatTime(user.CreatedAt)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages (id, knowledge_base_id, conversation_id, role, content, retrieval_trace_id, standalone_query, citations_json, context_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, assistant.ID, s.scope(ctx), assistant.ConversationID, assistant.Role, assistant.Content, assistant.RetrievalTraceID, assistant.StandaloneQuery, queryservice.EncodeSnapshots(assistant.Citations), queryservice.EncodeSnapshots(assistant.Context), formatTime(assistant.CreatedAt)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE conversations SET updated_at = ? WHERE id = ? AND knowledge_base_id = ?`, formatTime(assistant.CreatedAt), assistant.ConversationID, s.scope(ctx))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func historyCost(value string) int      { return (utf8.RuneCountInString(value) + 3) / 4 }
func formatTime(value time.Time) string { return value.Format(time.RFC3339Nano) }
func stableID(prefix, value string, now time.Time) string {
	sum := sha256.Sum256([]byte(value + now.Format(time.RFC3339Nano)))
	return prefix + "_" + hex.EncodeToString(sum[:12])
}
