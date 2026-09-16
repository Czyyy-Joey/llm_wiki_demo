package domain

import (
	"strings"
	"testing"
)

func TestValidationRejectsMissingRequiredFields(t *testing.T) {
	checks := []struct {
		name     string
		validate func() error
	}{
		{"source document", func() error { return (SourceDocument{}).Validate() }},
		{"source chunk", func() error { return (SourceChunk{}).Validate() }},
		{"wiki page", func() error { return (WikiPage{}).Validate() }},
		{"compilation plan", func() error { return (CompilationPlan{}).Validate() }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestCompilationPlanRejectsInvalidActionsAndEnums(t *testing.T) {
	plan := CompilationPlan{DocumentID: "doc_1", SchemaVersion: "1", PageActions: []PageAction{{Action: "INVALID", Reason: "reason"}}}
	if err := plan.Validate(); err == nil {
		t.Fatal("expected invalid page action error")
	} else if !strings.Contains(err.Error(), `field="action"`) || !strings.Contains(err.Error(), `value="INVALID"`) {
		t.Fatalf("page action diagnostic = %v", err)
	}
	page := WikiPage{ID: "page_1", Slug: "page", Title: "Page", Summary: "Summary", PageType: "invalid", Status: PageStatusActive}
	if err := page.Validate(); err == nil {
		t.Fatal("expected invalid page type error")
	} else if !strings.Contains(err.Error(), `field="page_type"`) || !strings.Contains(err.Error(), `value="invalid"`) {
		t.Fatalf("page diagnostic = %v", err)
	}
}

func TestCompilationPlanDiagnosticsIdentifyInvalidClaim(t *testing.T) {
	plan := CompilationPlan{
		DocumentID: "doc_1", SchemaVersion: "1",
		PageActions: []PageAction{{
			Action: ActionCreate, Slug: "topic", PageType: PageTypeTopic, Title: "Topic", Summary: "Summary", Reason: "test",
			ClaimActions: []ClaimAction{{Action: "ADD", Text: "A bounded claim summary", ClaimType: "not-a-claim", EvidenceChunkIDs: []string{"chunk_1"}}},
		}},
	}

	err := plan.Validate()
	if err == nil {
		t.Fatal("expected invalid claim action")
	}
	message := err.Error()
	for _, part := range []string{"invalid claim action", "page_index=0", "claim_index=0", `claim_type="not-a-claim"`, `text="A bounded claim summary"`, `field="claim_type"`, "unsupported claim type"} {
		if !strings.Contains(message, part) {
			t.Errorf("claim action diagnostic %q missing %q", message, part)
		}
	}
}

func TestCompilationPlanAcceptsValidAction(t *testing.T) {
	plan := CompilationPlan{DocumentID: "doc_1", SchemaVersion: "1", PageActions: []PageAction{{Action: ActionUpdate, TargetPageID: "page_1", Reason: "adds evidence", ClaimActions: []ClaimAction{{Action: "ADD", Text: "A claim", EvidenceChunkIDs: []string{"chunk_1"}}}}}}
	if err := plan.Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
}

func TestCompilationPlanRejectsUnsafeCreateSlugs(t *testing.T) {
	for _, slug := range []string{"", "../escape", `..\\escape`, "two words", "-leading", "trailing-", "double--dash", "UPPER"} {
		t.Run(slug, func(t *testing.T) {
			plan := CompilationPlan{
				DocumentID: "doc_1", SchemaVersion: "1",
				PageActions: []PageAction{{Action: ActionCreate, Slug: slug, PageType: PageTypeConcept, Title: "Title", Summary: "Summary", Reason: "test"}},
			}
			if err := plan.Validate(); err == nil {
				t.Fatalf("unsafe slug %q was accepted", slug)
			}
		})
	}
}
