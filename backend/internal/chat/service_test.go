package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeychen/llm-wiki-demo/backend/internal/db"
	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	queryservice "github.com/joeychen/llm-wiki-demo/backend/internal/query"
	"github.com/joeychen/llm-wiki-demo/backend/internal/retrieval"
)

type countingRetriever struct {
	calls []string
	db    *sql.DB
}

func (r *countingRetriever) Search(_ context.Context, query string) (retrieval.Result, error) {
	r.calls = append(r.calls, query)
	trace := domain.RetrievalTrace{ID: "trace-" + strings.ReplaceAll(query, " ", "-")}
	if r.db != nil {
		raw, _ := json.Marshal(trace)
		if _, err := r.db.Exec(`INSERT INTO retrieval_traces (id, normalized_query, trace_json, created_at) VALUES (?, ?, ?, '2024-01-01T00:00:00Z')`, trace.ID, query, string(raw)); err != nil {
			return retrieval.Result{}, err
		}
	}
	return retrieval.Result{Context: []retrieval.ContextItem{
		{ID: "wiki-context", Kind: "wiki", PageID: "page-1", PassageID: "passage-1", Text: "Vector databases store embeddings.", Citation: "vector-databases"},
		{ID: "source-chunk-1", Kind: "source_evidence", PageID: "page-1", SourceChunkID: "source-chunk-1", Text: "Vector databases store embeddings.", Citation: "source-chunk-1"},
	}, Trace: trace}, nil
}

type recordingGenerator struct {
	history []llm.ChatTurn
}

func (g *recordingGenerator) Rewrite(_ context.Context, question string, history []llm.ChatTurn) (json.RawMessage, error) {
	g.history = append([]llm.ChatTurn(nil), history...)
	return json.Marshal(domain.StandaloneQuery{Query: "Vector databases " + question})
}
func (g *recordingGenerator) Answer(_ context.Context, _ string, items []llm.AnswerContext) (json.RawMessage, error) {
	return json.Marshal(domain.GeneratedAnswer{Answer: "Vector databases store embeddings.", CitationIDs: []string{items[1].ID}})
}

func chatDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := db.Open(context.Background(), "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func TestChatUsesSharedRetrieverBoundedHistoryAndPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	database := chatDB(t, path)
	retriever := &countingRetriever{db: database}
	generator := &recordingGenerator{}
	service := Service{DB: database, Query: queryservice.Service{Retriever: retriever, Generator: generator}, Generator: generator, HistoryBudget: 20}
	conversation, err := service.Create(context.Background(), "Research")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), conversation.ID, "vector databases"); err != nil {
		t.Fatal(err)
	}
	turn, err := service.Send(context.Background(), conversation.ID, "what about it?")
	if err != nil {
		t.Fatal(err)
	}
	if len(retriever.calls) != 2 || retriever.calls[1] != "Vector databases what about it?" || turn.AssistantMessage.RetrievalTraceID == "" || len(turn.AssistantMessage.Citations) != 1 || turn.AssistantMessage.Citations[0].ID != "source-chunk-1" || turn.AssistantMessage.Citations[0].SourceChunkID != "source-chunk-1" || len(turn.AssistantMessage.Context) != 2 {
		t.Fatalf("turn = %#v, calls = %#v", turn, retriever.calls)
	}
	if len(generator.history) == 0 || len(generator.history) > 2 {
		t.Fatalf("history exceeded bounded recent budget: %#v", generator.history)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := chatDB(t, path)
	defer restarted.Close()
	recovered, err := (Service{DB: restarted}).Get(context.Background(), conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Messages) != 4 || recovered.Messages[3].RetrievalTraceID == "" || len(recovered.Messages[3].Citations) != 1 || recovered.Messages[3].Citations[0].ID != "source-chunk-1" || recovered.Messages[3].Citations[0].SourceChunkID != "source-chunk-1" || len(recovered.Messages[3].Context) != 2 || recovered.Messages[3].Context[1].ID != "source-chunk-1" {
		t.Fatalf("recovered conversation = %#v", recovered)
	}
}
