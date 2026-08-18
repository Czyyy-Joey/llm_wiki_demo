package domain

import (
	"fmt"
	"regexp"
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
		return fmt.Errorf("wiki page requires id, slug, title, and summary")
	}
	if !safeSlugPattern.MatchString(p.Slug) {
		return fmt.Errorf("wiki page has invalid slug")
	}
	if !validPageType(p.PageType) || !validPageStatus(p.Status) {
		return fmt.Errorf("wiki page has invalid page_type or status")
	}
	return nil
}

func (p CompilationPlan) Validate() error {
	if p.DocumentID == "" || p.SchemaVersion == "" {
		return fmt.Errorf("compilation plan requires document_id and schema_version")
	}
	for i, page := range p.PageActions {
		if !validPlanAction(page.Action) || page.Reason == "" {
			return fmt.Errorf("page action %d has invalid action or missing reason", i)
		}
		if page.Action != ActionCreate && page.Action != ActionNoOp && page.TargetPageID == "" {
			return fmt.Errorf("page action %d requires target_page_id", i)
		}
		if page.Action == ActionLink && page.SourcePageID == "" {
			return fmt.Errorf("page action %d LINK requires source_page_id", i)
		}
		if page.Action == ActionLink && page.Relation == "" {
			return fmt.Errorf("page action %d LINK requires relation", i)
		}
		if page.Action == ActionLink && !validLinkRelation(page.Relation) {
			return fmt.Errorf("page action %d LINK has invalid relation", i)
		}
		if page.Action == ActionLink && page.SourcePageID == page.TargetPageID {
			return fmt.Errorf("page action %d LINK cannot target the same page", i)
		}
		if page.Action == ActionMerge && page.SourcePageID == "" {
			return fmt.Errorf("page action %d MERGE requires source_page_id", i)
		}
		if page.Action == ActionMerge && page.SourcePageID == page.TargetPageID {
			return fmt.Errorf("page action %d MERGE cannot target the same page", i)
		}
		if page.Action == ActionCreate && (page.Slug == "" || !safeSlugPattern.MatchString(page.Slug) || page.Title == "" || page.Summary == "" || !validPageType(page.PageType)) {
			if page.Slug == "" || !safeSlugPattern.MatchString(page.Slug) {
				return fmt.Errorf("page action %d create has invalid slug", i)
			}
			return fmt.Errorf("page action %d create requires slug, title, summary, and valid page_type", i)
		}
		for j, claim := range page.ClaimActions {
			if !validClaimAction(claim.Action) || claim.Text == "" {
				return fmt.Errorf("claim action %d.%d has invalid action or missing text", i, j)
			}
			if claim.Action != "RETAIN" && len(claim.EvidenceChunkIDs) == 0 {
				return fmt.Errorf("claim action %d.%d requires evidence", i, j)
			}
			if (claim.Action == "REVISE" || claim.Action == "SUPERSEDE") && claim.TargetClaimID == "" && claim.PreviousText == "" {
				return fmt.Errorf("claim action %d.%d requires target_claim_id or previous_text", i, j)
			}
			if claim.ClaimType != "" && !validClaimType(claim.ClaimType) {
				return fmt.Errorf("claim action %d.%d has invalid claim_type", i, j)
			}
		}
	}
	return nil
}

func (t RetrievalTrace) Validate() error {
	if t.ID == "" || t.NormalizedQuery == "" {
		return fmt.Errorf("retrieval trace requires id and normalized_query")
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
