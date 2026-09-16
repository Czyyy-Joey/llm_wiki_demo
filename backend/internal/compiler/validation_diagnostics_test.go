package compiler

import (
	"strings"
	"testing"

	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
)

func TestValidateAnalysisIdentifiesInvalidTopic(t *testing.T) {
	analysis := domain.SourceAnalysis{
		DocumentID: "doc_1",
		Summary:    "summary",
		Topics: []domain.AnalyzedTopic{
			{Key: "valid", Title: "Valid", Slug: "valid", PageType: domain.PageTypeConcept, Summary: "summary"},
			{Key: "moving-plan", Title: "新家搬迁计划", Slug: "new-home-moving-plan", PageType: "plan", Summary: "summary"},
		},
	}

	err := validateAnalysis(analysis, nil)
	if err == nil {
		t.Fatal("expected invalid topic")
	}
	message := err.Error()
	for _, part := range []string{
		"invalid topic", "index=1", `key="moving-plan"`, `title="新家搬迁计划"`,
		`slug="new-home-moving-plan"`, `page_type="plan"`, `field="page_type"`,
		`value="plan"`, "one of concept, entity, topic", "unsupported page type",
	} {
		if !strings.Contains(message, part) {
			t.Errorf("topic diagnostic %q missing %q", message, part)
		}
	}
}

func TestValidateAnalysisIdentifiesInvalidClaimAndEvidence(t *testing.T) {
	analysis := domain.SourceAnalysis{
		DocumentID: "doc_1",
		Summary:    "summary",
		Topics: []domain.AnalyzedTopic{{
			Key: "topic", Title: "Topic", Slug: "topic", PageType: domain.PageTypeTopic, Summary: "summary",
			Claims: []domain.AnalyzedClaim{{
				Text:             strings.Repeat("claim text ", 40),
				ClaimType:        "unsupported",
				EvidenceChunkIDs: []string{"missing_chunk"},
			}},
		}},
	}

	err := validateAnalysis(analysis, nil)
	if err == nil {
		t.Fatal("expected invalid claim")
	}
	message := err.Error()
	for _, part := range []string{
		"invalid claim", "index=0", `topic_key="topic"`, `claim_type="unsupported"`,
		`evidence_chunk_ids=[missing_chunk]`, `field="claim_type"`,
		"one of fact, definition, argument, procedure, caveat", "unsupported claim type",
	} {
		if !strings.Contains(message, part) {
			t.Errorf("claim diagnostic %q missing %q", message, part)
		}
	}
	if len(message) > 900 {
		t.Fatalf("claim diagnostic is not bounded: %d bytes", len(message))
	}
}

func TestValidateAnalysisIdentifiesInvalidRelation(t *testing.T) {
	analysis := domain.SourceAnalysis{
		DocumentID: "doc_1",
		Summary:    "summary",
		Topics: []domain.AnalyzedTopic{
			{Key: "one", Title: "One", Slug: "one", PageType: domain.PageTypeConcept, Summary: "summary"},
			{Key: "two", Title: "Two", Slug: "two", PageType: domain.PageTypeTopic, Summary: "summary"},
		},
		Relations: []domain.AnalyzedRelation{{SourceTopicKey: "one", TargetTopicKey: "two", Relation: "made-up"}},
	}

	err := validateAnalysis(analysis, nil)
	if err == nil || !strings.Contains(err.Error(), `field="relation"`) || !strings.Contains(err.Error(), `value="made-up"`) {
		t.Fatalf("relation diagnostic = %v", err)
	}
}
