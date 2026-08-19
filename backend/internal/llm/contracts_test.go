package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
)

func TestDecodeStrict(t *testing.T) {
	valid := `{"document_id":"doc_1","page_actions":[],"schema_version":"1"}`
	plan, err := DecodeStrict[domain.CompilationPlan]([]byte(valid))
	if err != nil || plan.DocumentID != "doc_1" {
		t.Fatalf("valid plan: %#v, %v", plan, err)
	}

	for _, invalid := range []string{
		`{"document_id":"doc_1","page_actions":[],"schema_version":"1","unknown":true}`,
		valid + ` {}`,
	} {
		if _, err := DecodeStrict[domain.CompilationPlan]([]byte(invalid)); err == nil {
			t.Fatalf("expected strict decode failure for %s", invalid)
		}
	}
}

func TestSchemaForCompilationPlan(t *testing.T) {
	schema := SchemaFor[domain.CompilationPlan]()
	if schema == nil || schema.Type != "object" {
		t.Fatalf("unexpected schema: %#v", schema)
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"$ref"`) {
		t.Fatalf("schema contains unresolved references: %s", encoded)
	}
}

func TestDeterministicFakeGroupsTopicsByHeading(t *testing.T) {
	client := DeterministicFake{}
	data, err := client.Analyze(context.Background(), AnalyzeInput{
		Document: domain.SourceDocument{ID: "doc_1", OriginalName: "multi.md"},
		Chunks: []domain.SourceChunk{
			{ID: "chunk_a", Text: "Alpha is a concept.", HeadingPath: []string{"Alpha"}},
			{ID: "chunk_b", Text: "Alpha has a property.", HeadingPath: []string{"Alpha"}},
			{ID: "chunk_c", Text: "Beta is another concept.", HeadingPath: []string{"Beta"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := DecodeStrict[domain.SourceAnalysis](data)
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.Topics) != 2 || analysis.Topics[0].Key != "alpha" || analysis.Topics[1].Key != "beta" {
		t.Fatalf("topics = %#v", analysis.Topics)
	}
	if len(analysis.Topics[0].Claims) != 2 || len(analysis.Topics[1].Claims) != 1 {
		t.Fatalf("claims were not grouped by heading: %#v", analysis.Topics)
	}
}

func TestDeterministicFakePlansLinkAndMergeActions(t *testing.T) {
	client := DeterministicFake{}
	analysis := domain.SourceAnalysis{
		DocumentID: "doc_1",
		Summary:    "two related topics",
		Topics: []domain.AnalyzedTopic{
			{Key: "alpha", Title: "Alpha", Slug: "alpha", PageType: domain.PageTypeConcept, Summary: "alpha"},
			{Key: "beta", Title: "Beta", Slug: "beta", PageType: domain.PageTypeConcept, Summary: "beta"},
		},
		Relations: []domain.AnalyzedRelation{{SourceTopicKey: "alpha", TargetTopicKey: "beta", Relation: "related_to"}},
	}
	candidates := []domain.CompilationCandidate{
		{TopicKey: "alpha", PageID: "page_alpha", Title: "Alpha", Slug: "alpha", Score: 1},
		{TopicKey: "alpha", PageID: "page_alpha_duplicate", Title: "Alpha Duplicate", Slug: "alpha-duplicate", Score: 0.9},
		{TopicKey: "beta", PageID: "page_beta", Title: "Beta", Slug: "beta", Score: 1},
	}
	analysisRaw, _ := json.Marshal(analysis)
	candidatesRaw, _ := json.Marshal(candidates)
	planRaw, err := client.Plan(context.Background(), PlanInput{DocumentID: "doc_1", Analysis: analysisRaw, Candidates: candidatesRaw})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := DecodeStrict[domain.CompilationPlan](planRaw)
	if err != nil {
		t.Fatal(err)
	}
	var link, merge bool
	for _, action := range plan.PageActions {
		link = link || action.Action == domain.ActionLink && action.SourcePageID == "page_alpha" && action.TargetPageID == "page_beta"
		merge = merge || action.Action == domain.ActionMerge && action.SourcePageID == "page_alpha_duplicate" && action.TargetPageID == "page_alpha"
	}
	if !link || !merge {
		t.Fatalf("planner actions = %#v, want LINK and MERGE", plan.PageActions)
	}
}

func TestOpenAICompatibleUsesStrictStructuredOutput(t *testing.T) {
	var calls int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q", got)
		}
		var request struct {
			Model          string `json:"model"`
			ResponseFormat struct {
				Type       string `json:"type"`
				JSONSchema struct {
					Strict bool            `json:"strict"`
					Schema json.RawMessage `json:"schema"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.Model != "test-model" || request.ResponseFormat.Type != "json_schema" || !request.ResponseFormat.JSONSchema.Strict || strings.Contains(string(request.ResponseFormat.JSONSchema.Schema), `"$ref"`) {
			t.Errorf("structured request = %#v", request)
		}
		content := `{"document_id":"doc_1","summary":"summary","topics":[],"relations":[]}`
		if calls == 2 {
			content = `{"document_id":"doc_1","page_actions":[],"schema_version":"phase2-v1"}`
		}
		body := `{"choices":[{"message":{"content":` + strconv.Quote(content) + `}}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})

	client := NewOpenAICompatible("https://provider.example/v1", "test-key", "test-model", "")
	client.HTTP = &http.Client{Transport: transport}
	if _, err := client.Analyze(context.Background(), AnalyzeInput{Document: domain.SourceDocument{ID: "doc_1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Plan(context.Background(), PlanInput{DocumentID: "doc_1", Analysis: json.RawMessage(`{"document_id":"doc_1","summary":"summary","topics":[],"relations":[]}`), Candidates: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("OpenAI-compatible calls = %d, want 2", calls)
	}
}

func TestOpenAICompatibleFallsBackToJSONObjectWhenSchemaUnsupported(t *testing.T) {
	var calls int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		var request struct {
			Messages []chatMessage  `json:"messages"`
			Format   responseFormat `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			if request.Format.Type != "json_schema" || request.Format.JSONSchema == nil || !request.Format.JSONSchema.Strict {
				t.Fatalf("first response_format = %#v", request.Format)
			}
			return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"response_format json_schema is unsupported"}}`)), Request: r}, nil
		}
		if request.Format.Type != "json_object" || request.Format.JSONSchema != nil {
			t.Fatalf("fallback response_format = %#v", request.Format)
		}
		if len(request.Messages) == 0 || !strings.Contains(request.Messages[0].Content, "strictly matches this JSON Schema") {
			t.Fatalf("fallback system prompt = %#v", request.Messages)
		}
		content := `{"document_id":"doc_1","summary":"summary","topics":[],"relations":[]}`
		body := `{"choices":[{"message":{"content":` + strconv.Quote(content) + `}}]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})

	client := NewOpenAICompatible("https://oneapi.example/v1", "secret", "model", "")
	client.HTTP = &http.Client{Transport: transport}
	raw, err := client.Analyze(context.Background(), AnalyzeInput{Document: domain.SourceDocument{ID: "doc_1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeStrict[domain.SourceAnalysis](raw); err != nil {
		t.Fatalf("strict decode after fallback: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
