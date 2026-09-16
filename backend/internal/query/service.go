package query

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	"github.com/joeychen/llm-wiki-demo/backend/internal/logging"
	"github.com/joeychen/llm-wiki-demo/backend/internal/retrieval"
)

const InsufficientEvidence = "知识库证据不足，无法基于现有证据回答。"

type Retriever interface {
	Search(context.Context, string) (retrieval.Result, error)
}

type Service struct {
	Retriever Retriever
	Generator llm.GenerationClient
	Language  string
}

func insufficientEvidence(language string) string {
	if language == "en" {
		return "The knowledge base does not contain enough evidence to answer."
	}
	return InsufficientEvidence
}

type Result struct {
	Question       string                    `json:"question"`
	Answer         string                    `json:"answer"`
	Citations      []domain.CitationSnapshot `json:"citations"`
	WikiReferences []domain.CitationSnapshot `json:"wiki_references"`
	Context        []domain.CitationSnapshot `json:"context"`
	Trace          domain.RetrievalTrace     `json:"retrieval_trace"`
}

func (s Service) Ask(ctx context.Context, question string) (Result, error) {
	return s.AskWithRetrievalQuery(ctx, question, question)
}

// AskWithRetrievalQuery preserves the user's wording for answer generation
// while allowing Chat to retrieve with a standalone rewrite.
func (s Service) AskWithRetrievalQuery(ctx context.Context, question, retrievalQuery string) (Result, error) {
	ctx = llm.WithLanguage(ctx, s.Language)
	started := time.Now()
	question = strings.TrimSpace(question)
	if question == "" {
		return Result{}, fmt.Errorf("question is required")
	}
	retrievalQuery = strings.TrimSpace(retrievalQuery)
	if retrievalQuery == "" {
		return Result{}, fmt.Errorf("retrieval query is required")
	}
	if s.Retriever == nil {
		return Result{}, fmt.Errorf("retriever is not configured")
	}
	if s.Generator == nil {
		return Result{}, fmt.Errorf("chat generation provider is not configured")
	}
	retrieved, err := s.Retriever.Search(ctx, retrievalQuery)
	if err != nil {
		logging.Logger(ctx).Error("query retrieval failed", "error", logging.SafeSummary(err))
		return Result{}, err
	}
	logging.Logger(ctx).Info("query retrieval completed", "trace_id", retrieved.Trace.ID, "context_items", len(retrieved.Context))
	contextSnapshots := snapshots(retrieved.Context)
	result := Result{Question: question, Context: contextSnapshots, Trace: retrieved.Trace, Citations: []domain.CitationSnapshot{}, WikiReferences: wikiReferences(retrieved)}
	if len(retrieved.Context) == 0 {
		result.Answer = insufficientEvidence(s.Language)
		logging.Logger(ctx).Warn("query insufficient evidence", "trace_id", retrieved.Trace.ID, "duration_ms", logging.Duration(started))
		return result, nil
	}
	answerContext := make([]llm.AnswerContext, len(retrieved.Context))
	allowedSourceCitations := make(map[string]domain.CitationSnapshot, len(contextSnapshots))
	for i, item := range retrieved.Context {
		answerContext[i] = llm.AnswerContext{ID: item.ID, Kind: item.Kind, Text: item.Text}
		if item.SourceChunkID != "" {
			allowedSourceCitations[item.ID] = contextSnapshots[i]
		}
	}
	raw, err := s.Generator.Answer(ctx, question, answerContext)
	if err != nil {
		logging.Logger(ctx).Error("query answer generation failed", "trace_id", retrieved.Trace.ID, "error", logging.SafeSummary(err))
		return Result{}, err
	}
	generated, err := llm.DecodeStrict[domain.GeneratedAnswer](raw)
	if err != nil {
		return Result{}, err
	}
	seen := map[string]bool{}
	for _, id := range generated.CitationIDs {
		if citation, ok := allowedSourceCitations[id]; ok && !seen[id] {
			seen[id] = true
			result.Citations = append(result.Citations, citation)
		}
	}
	result.Answer = strings.TrimSpace(generated.Answer)
	if result.Answer == "" || len(result.Citations) == 0 {
		result.Answer = insufficientEvidence(s.Language)
		result.Citations = []domain.CitationSnapshot{}
	}
	logging.Logger(ctx).Info("query completed", "trace_id", retrieved.Trace.ID, "citations", len(result.Citations), "wiki_references", len(result.WikiReferences), "duration_ms", logging.Duration(started))
	return result, nil
}

func snapshots(items []retrieval.ContextItem) []domain.CitationSnapshot {
	result := make([]domain.CitationSnapshot, len(items))
	for i, item := range items {
		result[i] = domain.CitationSnapshot{ID: item.ID, Kind: item.Kind, PageID: item.PageID, PassageID: item.PassageID, SourceChunkID: item.SourceChunkID, Label: item.Citation, Text: item.Text}
	}
	return result
}

func wikiReferences(result retrieval.Result) []domain.CitationSnapshot {
	references := []domain.CitationSnapshot{}
	seen := map[string]bool{}
	for _, item := range result.Context {
		if item.PageID == "" || seen[item.PageID] {
			continue
		}
		seen[item.PageID] = true
		references = append(references, domain.CitationSnapshot{ID: item.PageID, Kind: "wiki_page", PageID: item.PageID, PassageID: item.PassageID, Label: item.Citation, Text: item.Text})
	}
	return references
}

func EncodeSnapshots(value []domain.CitationSnapshot) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
