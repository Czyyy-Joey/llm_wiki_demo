package domain

import (
	"fmt"
	"regexp"
	"strings"
)

var safeSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func (s SourceDocument) Validate() error {
	if s.ID == "" || s.OriginalName == "" || s.MediaType == "" || s.SHA256 == "" || s.OriginalPath == "" || s.Status == "" || s.ParserVersion == "" {
		return fmt.Errorf("source document requires id, original_name, media_type, sha256, original_path, status, and parser_version")
	}
	return nil
}

func (s SourceChunk) Validate() error {
	if s.ID == "" || s.DocumentID == "" || s.Text == "" || s.ContentHash == "" {
		return fmt.Errorf("source chunk requires id, document_id, text, and content_hash")
	}
	if s.ChunkIndex < 0 || s.CharStart < 0 || s.CharEnd < s.CharStart {
		return fmt.Errorf("source chunk has invalid position")
	}
	return nil
}

func (p WikiPage) Validate() error {
	if p.ID == "" || p.Slug == "" || p.Title == "" || p.Summary == "" {
		return fmt.Errorf("invalid wiki page: id=%q slug=%q title=%q field=%q reason=%q", p.ID, p.Slug, diagnosticText(p.Title), firstEmptyWikiPageField(p), "required field is missing")
	}
	if !safeSlugPattern.MatchString(p.Slug) {
		return fmt.Errorf("invalid wiki page: id=%q slug=%q title=%q field=%q value=%q constraint=%q reason=%q", p.ID, p.Slug, diagnosticText(p.Title), "slug", p.Slug, "^[a-z0-9]+(?:-[a-z0-9]+)*$", "must be a lowercase hyphen-separated slug")
	}
	if !validPageType(p.PageType) {
		return fmt.Errorf("invalid wiki page: id=%q slug=%q title=%q field=%q value=%q constraint=%q reason=%q", p.ID, p.Slug, diagnosticText(p.Title), "page_type", p.PageType, "one of concept, entity, topic", "unsupported page type")
	}
	if !validPageStatus(p.Status) {
		return fmt.Errorf("invalid wiki page: id=%q slug=%q title=%q field=%q value=%q constraint=%q reason=%q", p.ID, p.Slug, diagnosticText(p.Title), "status", p.Status, "one of active, merged, archived", "unsupported page status")
	}
	return nil
}

func (p CompilationPlan) Validate() error {
	if p.DocumentID == "" || p.SchemaVersion == "" {
		field := "document_id"
		if p.DocumentID != "" {
			field = "schema_version"
		}
		return fmt.Errorf("invalid compilation plan: field=%q value=%q reason=%q", field, map[string]string{"document_id": p.DocumentID, "schema_version": p.SchemaVersion}[field], "required field is missing")
	}
	for i, page := range p.PageActions {
		prefix := pageActionDiagnostic(i, page)
		if !validPlanAction(page.Action) {
			return fmt.Errorf("%s field=%q value=%q constraint=%q reason=%q", prefix, "action", page.Action, "one of CREATE, UPDATE, MERGE, LINK, NO_OP", "unsupported plan action")
		}
		if strings.TrimSpace(page.Reason) == "" {
			return fmt.Errorf("%s field=%q reason=%q", prefix, "reason", "must be non-empty")
		}
		if page.Action != ActionCreate && page.Action != ActionNoOp && page.TargetPageID == "" {
			return fmt.Errorf("%s field=%q reason=%q", prefix, "target_page_id", "required for this action")
		}
		if page.Action == ActionLink && page.SourcePageID == "" {
			return fmt.Errorf("%s field=%q reason=%q", prefix, "source_page_id", "required for LINK")
		}
		if page.Action == ActionLink && page.Relation == "" {
			return fmt.Errorf("%s field=%q reason=%q", prefix, "relation", "required for LINK")
		}
		if page.Action == ActionLink && !validLinkRelation(page.Relation) {
			return fmt.Errorf("%s field=%q value=%q constraint=%q reason=%q", prefix, "relation", page.Relation, "one of related_to, part_of, depends_on, contradicts, supports, references", "unsupported link relation")
		}
		if page.Action == ActionLink && page.SourcePageID == page.TargetPageID {
			return fmt.Errorf("%s field=%q value=%q reason=%q", prefix, "target_page_id", page.TargetPageID, "must differ from source_page_id")
		}
		if page.Action == ActionMerge && page.SourcePageID == "" {
			return fmt.Errorf("%s field=%q reason=%q", prefix, "source_page_id", "required for MERGE")
		}
		if page.Action == ActionMerge && page.SourcePageID == page.TargetPageID {
			return fmt.Errorf("%s field=%q value=%q reason=%q", prefix, "target_page_id", page.TargetPageID, "must differ from source_page_id")
		}
		if page.Action == ActionCreate && (page.Slug == "" || !safeSlugPattern.MatchString(page.Slug) || page.Title == "" || page.Summary == "" || !validPageType(page.PageType)) {
			if page.Slug == "" || !safeSlugPattern.MatchString(page.Slug) {
				return fmt.Errorf("%s field=%q value=%q constraint=%q reason=%q", prefix, "slug", page.Slug, "^[a-z0-9]+(?:-[a-z0-9]+)*$", "invalid slug; must be a lowercase hyphen-separated slug")
			}
			field := "title"
			value := page.Title
			reason := "must be non-empty"
			if strings.TrimSpace(page.Title) == "" {
				field, value = "title", page.Title
			} else if strings.TrimSpace(page.Summary) == "" {
				field, value = "summary", page.Summary
			} else {
				field, value, reason = "page_type", string(page.PageType), "must be one of concept, entity, topic"
			}
			return fmt.Errorf("%s field=%q value=%q reason=%q", prefix, field, diagnosticText(value), reason)
		}
		for j, claim := range page.ClaimActions {
			claimPrefix := claimActionDiagnostic(i, j, page, claim)
			if !validClaimAction(claim.Action) {
				return fmt.Errorf("%s field=%q value=%q constraint=%q reason=%q", claimPrefix, "action", claim.Action, "one of ADD, REVISE, RETAIN, MARK_DISPUTED, SUPERSEDE", "unsupported claim action")
			}
			if strings.TrimSpace(claim.Text) == "" {
				return fmt.Errorf("%s field=%q reason=%q", claimPrefix, "text", "must be non-empty")
			}
			if claim.Action != "RETAIN" && len(claim.EvidenceChunkIDs) == 0 {
				return fmt.Errorf("%s field=%q value=%v reason=%q", claimPrefix, "evidence_chunk_ids", claim.EvidenceChunkIDs, "must contain at least one source chunk ID")
			}
			if (claim.Action == "REVISE" || claim.Action == "SUPERSEDE") && claim.TargetClaimID == "" && claim.PreviousText == "" {
				return fmt.Errorf("%s field=%q reason=%q", claimPrefix, "target_claim_id/previous_text", "one is required for REVISE or SUPERSEDE")
			}
			if claim.ClaimType != "" && !validClaimType(claim.ClaimType) {
				return fmt.Errorf("%s field=%q value=%q constraint=%q reason=%q", claimPrefix, "claim_type", claim.ClaimType, "one of fact, definition, argument, procedure, caveat", "unsupported claim type")
			}
		}
	}
	return nil
}

func pageActionDiagnostic(index int, page PageAction) string {
	return fmt.Sprintf("invalid page action: index=%d action=%q target_page_id=%q source_page_id=%q slug=%q title=%q", index, page.Action, page.TargetPageID, page.SourcePageID, page.Slug, diagnosticText(page.Title))
}

func claimActionDiagnostic(pageIndex, claimIndex int, page PageAction, claim ClaimAction) string {
	return fmt.Sprintf("invalid claim action: page_index=%d claim_index=%d page_action=%q target_page_id=%q claim_type=%q text=%q evidence_chunk_ids=%v", pageIndex, claimIndex, page.Action, page.TargetPageID, claim.ClaimType, diagnosticText(claim.Text), claim.EvidenceChunkIDs)
}

func firstEmptyWikiPageField(page WikiPage) string {
	if page.ID == "" {
		return "id"
	}
	if page.Slug == "" {
		return "slug"
	}
	if page.Title == "" {
		return "title"
	}
	return "summary"
}

func diagnosticText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	const limit = 160
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "..."
	}
	return value
}

func (t RetrievalTrace) Validate() error {
	if t.ID == "" || t.NormalizedQuery == "" {
		field := "id"
		if t.ID != "" {
			field = "normalized_query"
		}
		return fmt.Errorf("invalid retrieval trace: field=%q value=%q reason=%q", field, map[string]string{"id": t.ID, "normalized_query": t.NormalizedQuery}[field], "required field is missing")
	}
	for index, candidate := range t.Candidates {
		if candidate.PageID == "" || candidate.PassageID == "" {
			field := "page_id"
			if candidate.PageID != "" {
				field = "passage_id"
			}
			return fmt.Errorf("invalid retrieval candidate: index=%d page_id=%q passage_id=%q field=%q value=%q reason=%q", index, candidate.PageID, candidate.PassageID, field, map[string]string{"page_id": candidate.PageID, "passage_id": candidate.PassageID}[field], "required field is missing")
		}
	}
	return nil
}

func validPageType(value PageType) bool {
	return value == PageTypeConcept || value == PageTypeEntity || value == PageTypeTopic
}
func validPageStatus(value PageStatus) bool {
	return value == PageStatusActive || value == PageStatusMerged || value == PageStatusArchived
}
func validPlanAction(value PlanAction) bool {
	return value == ActionCreate || value == ActionUpdate || value == ActionMerge || value == ActionLink || value == ActionNoOp
}
func validClaimAction(value string) bool {
	switch value {
	case "ADD", "REVISE", "RETAIN", "MARK_DISPUTED", "SUPERSEDE":
		return true
	default:
		return false
	}
}
func validClaimType(value ClaimType) bool {
	return value == ClaimFact || value == ClaimDefinition || value == ClaimArgument || value == ClaimProcedure || value == ClaimCaveat
}

func validLinkRelation(value string) bool {
	switch value {
	case "related_to", "part_of", "depends_on", "contradicts", "supports", "references":
		return true
	default:
		return false
	}
}
