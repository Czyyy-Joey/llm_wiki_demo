package query

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	"github.com/joeychen/llm-wiki-demo/backend/internal/retrieval"
)

type stubRetriever struct {
	calls  int
	result retrieval.Result
}

func (r *stubRetriever) Search(_ context.Context, query string) (retrieval.Result, error) {
	r.calls++
	r.result.Query = query
	return r.result, nil
}

type answerClient struct{ answer domain.GeneratedAnswer }

func (c answerClient) Rewrite(context.Context, string, []llm.ChatTurn) (json.RawMessage, error) {
	return json.Marshal(domain.StandaloneQuery{Query: "unused"})
}
func (c answerClient) Answer(context.Context, string, []llm.AnswerContext) (json.RawMessage, error) {
	return json.Marshal(c.answer)
}

func TestAskOnlyAcceptsCitationsFromCurrentContext(t *testing.T) {
	retriever := &stubRetriever{result: retrieval.Result{Context: []retrieval.ContextItem{{ID: "chunk_real", Kind: "source_evidence", PageID: "page_1", SourceChunkID: "chunk_real", Text: "Grounded fact", Citation: "chunk_real"}}, Trace: domain.RetrievalTrace{ID: "trace_1"}}}
	service := Service{Retriever: retriever, Generator: answerClient{answer: domain.GeneratedAnswer{Answer: "Grounded fact", CitationIDs: []string{"chunk_fake", "chunk_real", "chunk_real"}}}}
	result, err := service.Ask(context.Background(), "What is grounded?")
	if err != nil {
		t.Fatal(err)
	}
	if retriever.calls != 1 || len(result.Citations) != 1 || result.Citations[0].ID != "chunk_real" || result.Trace.ID != "trace_1" {
		t.Fatalf("result = %#v, retriever calls = %d", result, retriever.calls)
	}
}

func TestAskRejectsFullyForgedCitationsAndHandlesNoEvidence(t *testing.T) {
	withContext := &stubRetriever{result: retrieval.Result{Context: []retrieval.ContextItem{{ID: "wiki_1", Kind: "wiki", Text: "Fact"}}}}
	result, err := (Service{Retriever: withContext, Generator: answerClient{answer: domain.GeneratedAnswer{Answer: "Invented", CitationIDs: []string{"fake"}}}}).Ask(context.Background(), "question")
	if err != nil || result.Answer != InsufficientEvidence || len(result.Citations) != 0 {
		t.Fatalf("forged citation result = %#v, err = %v", result, err)
	}
	empty := &stubRetriever{result: retrieval.Result{Context: []retrieval.ContextItem{}, Trace: domain.RetrievalTrace{ID: "trace_empty"}}}
	result, err = (Service{Retriever: empty, Generator: answerClient{answer: domain.GeneratedAnswer{Answer: "must not run"}}}).Ask(context.Background(), "unknown")
	if err != nil || result.Answer != InsufficientEvidence || result.Trace.ID != "trace_empty" {
		t.Fatalf("empty result = %#v, err = %v", result, err)
	}
}
