package wiki

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/knowledgebase"
)

type Service struct {
	DB              *sql.DB
	DataRoot        string
	KnowledgeBaseID string
}

func (s Service) scope(ctx context.Context) string {
	return knowledgebase.Scope(ctx, s.KnowledgeBaseID)
}

type PageListItem struct {
	domain.WikiPage
	ClaimCount  int `json:"claim_count"`
	SourceCount int `json:"source_count"`
	LinkCount   int `json:"link_count"`
}

type EvidenceView struct {
	Evidence domain.ClaimEvidence  `json:"evidence"`
	Chunk    domain.SourceChunk    `json:"chunk"`
	Source   domain.SourceDocument `json:"source"`
}

type ClaimView struct {
	Claim    domain.WikiClaim `json:"claim"`
	Evidence []EvidenceView   `json:"evidence"`
}

type SectionView struct {
	Section domain.WikiSection `json:"section"`
	Claims  []ClaimView        `json:"claims"`
}

type LinkView struct {
	Link domain.WikiLink `json:"link"`
	Page domain.WikiPage `json:"page"`
}

type SourceTrace struct {
	Source       domain.SourceDocument `json:"source"`
	ChunkCount   int                   `json:"chunk_count"`
	ClaimCount   int                   `json:"claim_count"`
	EvidenceHits []EvidenceView        `json:"evidence"`
}

type PageDetail struct {
	Page          domain.WikiPage  `json:"page"`
	CanonicalPage *domain.WikiPage `json:"canonical_page,omitempty"`
	Sections      []SectionView    `json:"sections"`
	Claims        []ClaimView      `json:"claims"`
	Links         []LinkView       `json:"links"`
	Backlinks     []LinkView       `json:"backlinks"`
	Related       []LinkView       `json:"related"`
	Sources       []SourceTrace    `json:"sources"`
}

type RevisionView struct {
	Revision domain.WikiRevision `json:"revision"`
	Diff     RevisionDiff        `json:"diff"`
}

type RevisionDiff struct {
	AddedClaims   []string `json:"added_claims"`
	RemovedClaims []string `json:"removed_claims"`
	Changed       []string `json:"changed"`
}

type LintIssue struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	EntityID string `json:"entity_id,omitempty"`
	Message  string `json:"message"`
}

func (s Service) ListPages(ctx context.Context, pageType string) ([]PageListItem, error) {
	query := `SELECT p.id, p.slug, p.page_type, p.title, p.summary, p.status, p.current_revision, p.created_at, p.updated_at,
	                 (SELECT COUNT(*) FROM wiki_claims c WHERE c.page_id = p.id AND c.knowledge_base_id = p.knowledge_base_id AND c.status != 'superseded'),
	                 (SELECT COUNT(DISTINCT sc.document_id) FROM wiki_claims c JOIN claim_evidence ce ON ce.claim_id = c.id AND ce.knowledge_base_id = c.knowledge_base_id JOIN source_chunks sc ON sc.id = ce.source_chunk_id AND sc.knowledge_base_id = c.knowledge_base_id WHERE c.page_id = p.id AND c.knowledge_base_id = p.knowledge_base_id),
				 (SELECT COUNT(*) FROM wiki_links l JOIN wiki_pages lsp ON lsp.id = l.source_page_id AND lsp.status = 'active' AND lsp.knowledge_base_id = l.knowledge_base_id JOIN wiki_pages ltp ON ltp.id = l.target_page_id AND ltp.status = 'active' AND ltp.knowledge_base_id = l.knowledge_base_id WHERE l.knowledge_base_id = p.knowledge_base_id AND (l.source_page_id = p.id OR l.target_page_id = p.id))
				 FROM wiki_pages p WHERE p.status = 'active' AND p.knowledge_base_id = ?`
	args := []any{s.scope(ctx)}
	if pageType != "" {
		query += ` AND p.page_type = ?`
		args = append(args, pageType)
	}
	query += ` ORDER BY p.page_type, p.slug`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]PageListItem, 0)
	for rows.Next() {
		item, err := scanPageList(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s Service) GetPage(ctx context.Context, key string) (PageDetail, error) {
	page, err := s.resolvePage(ctx, key)
	if err != nil {
		return PageDetail{}, err
	}
	if page.Status != domain.PageStatusActive {
		canonical, canonicalErr := s.canonicalPage(ctx, page.ID)
		if canonicalErr != nil {
			return PageDetail{}, canonicalErr
		}
		return PageDetail{Page: page, CanonicalPage: canonical, Sections: []SectionView{}, Claims: []ClaimView{}, Links: []LinkView{}, Backlinks: []LinkView{}, Related: []LinkView{}, Sources: []SourceTrace{}}, nil
	}
	claims, err := s.claimViews(ctx, page.ID)
	if err != nil {
		return PageDetail{}, err
	}
	sections, err := s.sections(ctx, page.ID, claims)
	if err != nil {
		return PageDetail{}, err
	}
	links, err := s.links(ctx, page.ID, true)
	if err != nil {
		return PageDetail{}, err
	}
	backlinks, err := s.links(ctx, page.ID, false)
	if err != nil {
		return PageDetail{}, err
	}
	related := make([]LinkView, 0, len(links)+len(backlinks))
	seen := map[string]bool{}
	for _, item := range append(append([]LinkView{}, links...), backlinks...) {
		if !seen[item.Page.ID] {
			seen[item.Page.ID] = true
			related = append(related, item)
		}
	}
	sources, err := s.pageSources(ctx, page.ID)
	if err != nil {
		return PageDetail{}, err
	}
	return PageDetail{Page: page, Sections: sections, Claims: claims, Links: links, Backlinks: backlinks, Related: related, Sources: sources}, nil
}

type GraphNode struct {
	ID          string          `json:"id"`
	Slug        string          `json:"slug"`
	Title       string          `json:"title"`
	PageType    domain.PageType `json:"page_type"`
	Summary     string          `json:"summary"`
	ClaimCount  int             `json:"claim_count"`
	SourceCount int             `json:"source_count"`
	LinkCount   int             `json:"link_count"`
}

type GraphEdge struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	Relation string `json:"relation"`
}

type WikiGraph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// Graph returns every active Wiki page as a node and every link between two
// active pages as a directed edge, scoped to the current knowledge base. Links
// to non-active pages (e.g. merged_into) are excluded so the graph only shows
// documents a reader can open.
func (s Service) Graph(ctx context.Context) (WikiGraph, error) {
	pages, err := s.ListPages(ctx, "")
	if err != nil {
		return WikiGraph{}, err
	}
	nodes := make([]GraphNode, 0, len(pages))
	for _, page := range pages {
		nodes = append(nodes, GraphNode{
			ID: page.ID, Slug: page.Slug, Title: page.Title, PageType: page.PageType, Summary: page.Summary,
			ClaimCount: page.ClaimCount, SourceCount: page.SourceCount, LinkCount: page.LinkCount,
		})
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT l.source_page_id, l.target_page_id, l.relation
		FROM wiki_links l
		JOIN wiki_pages sp ON sp.id = l.source_page_id AND sp.status = 'active' AND sp.knowledge_base_id = l.knowledge_base_id
		JOIN wiki_pages tp ON tp.id = l.target_page_id AND tp.status = 'active' AND tp.knowledge_base_id = l.knowledge_base_id
		WHERE l.knowledge_base_id = ? AND l.relation != 'merged_into'
		ORDER BY l.source_page_id, l.target_page_id, l.relation`, s.scope(ctx))
	if err != nil {
		return WikiGraph{}, err
	}
	defer rows.Close()
	edges := make([]GraphEdge, 0)
	for rows.Next() {
		var edge GraphEdge
		if err := rows.Scan(&edge.Source, &edge.Target, &edge.Relation); err != nil {
			return WikiGraph{}, err
		}
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		return WikiGraph{}, err
	}
	return WikiGraph{Nodes: nodes, Edges: edges}, nil
}

func (s Service) SourceTrace(ctx context.Context, documentID string) (SourceTrace, error) {
	var source domain.SourceDocument
	var created string
	err := s.DB.QueryRowContext(ctx, `SELECT id, original_name, media_type, sha256, original_path, COALESCE(parsed_path, ''), status, parser_version, created_at, COALESCE(parse_error, '') FROM source_documents WHERE id = ? AND knowledge_base_id = ?`, documentID, s.scope(ctx)).
		Scan(&source.ID, &source.OriginalName, &source.MediaType, &source.SHA256, &source.OriginalPath, &source.ParsedPath, &source.Status, &source.ParserVersion, &created, &source.ParseError)
	if err != nil {
		return SourceTrace{}, err
	}
	source.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	var chunks int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM source_chunks WHERE document_id = ? AND knowledge_base_id = ?`, documentID, s.scope(ctx)).Scan(&chunks); err != nil {
		return SourceTrace{}, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT ce.claim_id, ce.source_chunk_id, ce.relation, COALESCE(ce.note, ''),
       sc.id, sc.document_id, sc.chunk_index, sc.text, sc.page_number, sc.heading_path, sc.char_start, sc.char_end, sc.content_hash
	   FROM wiki_claims c JOIN wiki_pages p ON p.id = c.page_id AND p.status = 'active' AND p.knowledge_base_id = c.knowledge_base_id JOIN claim_evidence ce ON ce.claim_id = c.id AND ce.knowledge_base_id = c.knowledge_base_id JOIN source_chunks sc ON sc.id = ce.source_chunk_id AND sc.knowledge_base_id = c.knowledge_base_id
	   WHERE sc.document_id = ? AND c.knowledge_base_id = ? ORDER BY c.page_id, c.id, ce.source_chunk_id`, documentID, s.scope(ctx))
	if err != nil {
		return SourceTrace{}, err
	}
	defer rows.Close()
	hits := make([]EvidenceView, 0)
	claimSet := map[string]bool{}
	for rows.Next() {
		view, err := scanEvidenceView(rows, source)
		if err != nil {
			return SourceTrace{}, err
		}
		hits = append(hits, view)
		claimSet[view.Evidence.ClaimID] = true
	}
	if err := rows.Err(); err != nil {
		return SourceTrace{}, err
	}
	return SourceTrace{Source: source, ChunkCount: chunks, ClaimCount: len(claimSet), EvidenceHits: hits}, nil
}

func (s Service) Revisions(ctx context.Context, key string) ([]RevisionView, error) {
	page, err := s.resolvePage(ctx, key)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT page_id, revision_number, compilation_run_id, snapshot_json, change_summary, created_at FROM wiki_revisions WHERE page_id = ? AND knowledge_base_id = ? ORDER BY revision_number DESC`, page.ID, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type revisionSnapshot struct {
		revision domain.WikiRevision
		snapshot map[string]any
	}
	var snapshots []revisionSnapshot
	for rows.Next() {
		var revision domain.WikiRevision
		var snapshot, created string
		if err := rows.Scan(&revision.PageID, &revision.RevisionNumber, &revision.CompilationRunID, &snapshot, &revision.ChangeSummary, &created); err != nil {
			return nil, err
		}
		revision.Snapshot = json.RawMessage(snapshot)
		revision.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		var current map[string]any
		_ = json.Unmarshal(revision.Snapshot, &current)
		snapshots = append(snapshots, revisionSnapshot{revision: revision, snapshot: current})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]RevisionView, 0, len(snapshots))
	for _, item := range snapshots {
		var previous map[string]any
		for _, older := range snapshots {
			if older.revision.RevisionNumber == item.revision.RevisionNumber-1 {
				previous = older.snapshot
				break
			}
		}
		result = append(result, RevisionView{Revision: item.revision, Diff: diffSnapshots(item.snapshot, previous)})
	}
	return result, nil
}

func (s Service) Diff(ctx context.Context, key string, revisionNumber int) (RevisionDiff, error) {
	page, err := s.resolvePage(ctx, key)
	if err != nil {
		return RevisionDiff{}, err
	}
	var current, previous string
	err = s.DB.QueryRowContext(ctx, `SELECT snapshot_json FROM wiki_revisions WHERE page_id = ? AND revision_number = ? AND knowledge_base_id = ?`, page.ID, revisionNumber, s.scope(ctx)).Scan(&current)
	if err != nil {
		return RevisionDiff{}, err
	}
	if revisionNumber > 1 {
		_ = s.DB.QueryRowContext(ctx, `SELECT snapshot_json FROM wiki_revisions WHERE page_id = ? AND revision_number = ? AND knowledge_base_id = ?`, page.ID, revisionNumber-1, s.scope(ctx)).Scan(&previous)
	}
	var currentJSON, previousJSON map[string]any
	_ = json.Unmarshal([]byte(current), &currentJSON)
	_ = json.Unmarshal([]byte(previous), &previousJSON)
	return diffSnapshots(currentJSON, previousJSON), nil
}

func (s Service) Lint(ctx context.Context) ([]LintIssue, error) {
	issues := make([]LintIssue, 0)
	rows, err := s.DB.QueryContext(ctx, `SELECT l.source_page_id, l.target_page_id, l.relation FROM wiki_links l LEFT JOIN wiki_pages sp ON sp.id = l.source_page_id AND sp.knowledge_base_id = l.knowledge_base_id LEFT JOIN wiki_pages tp ON tp.id = l.target_page_id AND tp.knowledge_base_id = l.knowledge_base_id WHERE l.knowledge_base_id = ? AND (sp.id IS NULL OR tp.id IS NULL)`, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var source, target, relation string
		if err := rows.Scan(&source, &target, &relation); err != nil {
			rows.Close()
			return nil, err
		}
		issues = append(issues, LintIssue{Code: "broken_link", Severity: "error", EntityID: source, Message: fmt.Sprintf("link %s -> %s (%s) points to a missing page", source, target, relation)})
	}
	rows.Close()
	rows, err = s.DB.QueryContext(ctx, `SELECT l.source_page_id, l.target_page_id, l.relation, COALESCE(sp.status, ''), COALESCE(tp.status, '')
		FROM wiki_links l LEFT JOIN wiki_pages sp ON sp.id = l.source_page_id AND sp.knowledge_base_id = l.knowledge_base_id LEFT JOIN wiki_pages tp ON tp.id = l.target_page_id AND tp.knowledge_base_id = l.knowledge_base_id
		WHERE l.knowledge_base_id = ? AND (COALESCE(sp.status, '') != 'active' OR COALESCE(tp.status, '') != 'active') AND l.relation != 'merged_into'
		ORDER BY l.source_page_id, l.target_page_id, l.relation`, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var source, target, relation, sourceStatus, targetStatus string
		if err := rows.Scan(&source, &target, &relation, &sourceStatus, &targetStatus); err != nil {
			rows.Close()
			return nil, err
		}
		issues = append(issues, LintIssue{Code: "inactive_page_link", Severity: "error", EntityID: source, Message: fmt.Sprintf("link %s -> %s (%s) exposes non-active pages (%s -> %s)", source, target, relation, sourceStatus, targetStatus)})
	}
	rows.Close()
	rows, err = s.DB.QueryContext(ctx, `SELECT p.id FROM wiki_pages p LEFT JOIN wiki_links l ON l.source_page_id = p.id AND l.relation = 'merged_into' AND l.knowledge_base_id = p.knowledge_base_id LEFT JOIN wiki_pages target ON target.id = l.target_page_id AND target.status = 'active' AND target.knowledge_base_id = p.knowledge_base_id WHERE p.status = 'merged' AND p.knowledge_base_id = ? GROUP BY p.id HAVING COUNT(target.id) != 1`, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var pageID string
		if err := rows.Scan(&pageID); err != nil {
			rows.Close()
			return nil, err
		}
		issues = append(issues, LintIssue{Code: "merged_page_without_canonical", Severity: "error", EntityID: pageID, Message: "merged page must point to exactly one active canonical page"})
	}
	rows.Close()
	rows, err = s.DB.QueryContext(ctx, `SELECT c.id FROM wiki_claims c JOIN wiki_pages p ON p.id = c.page_id AND p.knowledge_base_id = c.knowledge_base_id WHERE p.status = 'merged' AND c.status IN ('active', 'disputed') AND c.knowledge_base_id = ?`, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var claimID string
		if err := rows.Scan(&claimID); err != nil {
			rows.Close()
			return nil, err
		}
		issues = append(issues, LintIssue{Code: "merged_page_claim", Severity: "error", EntityID: claimID, Message: "active claim remains on a merged page"})
	}
	rows.Close()
	if err := s.lintProjection(ctx, &issues); err != nil {
		return nil, err
	}
	rows, err = s.DB.QueryContext(ctx, `SELECT c.id, c.page_id FROM wiki_claims c LEFT JOIN claim_evidence ce ON ce.claim_id = c.id AND ce.knowledge_base_id = c.knowledge_base_id WHERE c.status IN ('active', 'disputed') AND c.knowledge_base_id = ? GROUP BY c.id, c.page_id HAVING COUNT(ce.source_chunk_id) = 0`, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var claim, page string
		if err := rows.Scan(&claim, &page); err != nil {
			rows.Close()
			return nil, err
		}
		issues = append(issues, LintIssue{Code: "claim_without_evidence", Severity: "error", EntityID: claim, Message: fmt.Sprintf("claim on page %s has no source evidence", page)})
	}
	rows.Close()
	rows, err = s.DB.QueryContext(ctx, `SELECT ce.claim_id, ce.source_chunk_id FROM claim_evidence ce LEFT JOIN wiki_claims c ON c.id = ce.claim_id AND c.knowledge_base_id = ce.knowledge_base_id LEFT JOIN source_chunks sc ON sc.id = ce.source_chunk_id AND sc.knowledge_base_id = ce.knowledge_base_id WHERE ce.knowledge_base_id = ? AND (c.id IS NULL OR sc.id IS NULL)`, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var claim, chunk string
		if err := rows.Scan(&claim, &chunk); err != nil {
			rows.Close()
			return nil, err
		}
		issues = append(issues, LintIssue{Code: "orphan_evidence", Severity: "error", EntityID: claim, Message: fmt.Sprintf("evidence references claim %s or missing chunk %s", claim, chunk)})
	}
	rows.Close()
	rows, err = s.DB.QueryContext(ctx, `SELECT slug, COUNT(*) FROM wiki_pages WHERE knowledge_base_id = ? GROUP BY slug HAVING COUNT(*) > 1`, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var slug, count string
		if err := rows.Scan(&slug, &count); err != nil {
			rows.Close()
			return nil, err
		}
		issues = append(issues, LintIssue{Code: "duplicate_slug", Severity: "error", EntityID: slug, Message: fmt.Sprintf("slug %s appears %s times", slug, count)})
	}
	rows.Close()
	sort.Slice(issues, func(i, j int) bool { return issues[i].Code+issues[i].EntityID < issues[j].Code+issues[j].EntityID })
	return issues, nil
}

func scanPageList(row interface{ Scan(...any) error }) (PageListItem, error) {
	var item PageListItem
	var created, updated string
	err := row.Scan(&item.ID, &item.Slug, &item.PageType, &item.Title, &item.Summary, &item.Status, &item.CurrentRevision, &created, &updated, &item.ClaimCount, &item.SourceCount, &item.LinkCount)
	item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return item, err
}

func (s Service) resolvePage(ctx context.Context, key string) (domain.WikiPage, error) {
	var page domain.WikiPage
	var created, updated string
	err := s.DB.QueryRowContext(ctx, `SELECT id, slug, page_type, title, summary, status, current_revision, created_at, updated_at FROM wiki_pages WHERE knowledge_base_id = ? AND (id = ? OR slug = ?)`, s.scope(ctx), key, key).Scan(&page.ID, &page.Slug, &page.PageType, &page.Title, &page.Summary, &page.Status, &page.CurrentRevision, &created, &updated)
	if err != nil {
		return page, err
	}
	page.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	page.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return page, nil
}

func (s Service) canonicalPage(ctx context.Context, pageID string) (*domain.WikiPage, error) {
	var page domain.WikiPage
	var created, updated string
	err := s.DB.QueryRowContext(ctx, `SELECT p.id, p.slug, p.page_type, p.title, p.summary, p.status, p.current_revision, p.created_at, p.updated_at
		FROM wiki_links l JOIN wiki_pages p ON p.id = l.target_page_id AND p.knowledge_base_id = l.knowledge_base_id
		WHERE l.source_page_id = ? AND l.relation = 'merged_into' AND p.status = 'active' AND l.knowledge_base_id = ? LIMIT 1`, pageID, s.scope(ctx)).
		Scan(&page.ID, &page.Slug, &page.PageType, &page.Title, &page.Summary, &page.Status, &page.CurrentRevision, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	page.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	page.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &page, nil
}

func (s Service) claimViews(ctx context.Context, pageID string) ([]ClaimView, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT c.id, c.page_id, COALESCE(c.section_id, ''), c.text, c.claim_type, c.status, c.created_by_run_id, c.updated_by_run_id,
	       COALESCE(ce.source_chunk_id, ''), COALESCE(ce.relation, ''), COALESCE(ce.note, ''), COALESCE(sc.id, ''), COALESCE(sc.document_id, ''), COALESCE(sc.chunk_index, 0), COALESCE(sc.text, ''), sc.page_number, COALESCE(sc.heading_path, '[]'), COALESCE(sc.char_start, 0), COALESCE(sc.char_end, 0), COALESCE(sc.content_hash, ''),
	       COALESCE(sd.id, ''), COALESCE(sd.original_name, ''), COALESCE(sd.media_type, ''), COALESCE(sd.sha256, ''), COALESCE(sd.original_path, ''), COALESCE(sd.parsed_path, ''), COALESCE(sd.status, ''), COALESCE(sd.parser_version, ''), COALESCE(sd.created_at, ''), COALESCE(sd.parse_error, '')
	       FROM wiki_claims c LEFT JOIN claim_evidence ce ON ce.claim_id = c.id AND ce.knowledge_base_id = c.knowledge_base_id LEFT JOIN source_chunks sc ON sc.id = ce.source_chunk_id AND sc.knowledge_base_id = c.knowledge_base_id LEFT JOIN source_documents sd ON sd.id = sc.document_id AND sd.knowledge_base_id = c.knowledge_base_id
	       WHERE c.page_id = ? AND c.knowledge_base_id = ? ORDER BY c.id, ce.source_chunk_id`, pageID, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string]int{}
	result := make([]ClaimView, 0)
	for rows.Next() {
		var claim domain.WikiClaim
		var section, chunkID, relation, note sql.NullString
		var chunk domain.SourceChunk
		var chunkDocID sql.NullString
		var page sql.NullInt64
		var heading sql.NullString
		var source domain.SourceDocument
		var sourceCreated sql.NullString
		if err := rows.Scan(&claim.ID, &claim.PageID, &section, &claim.Text, &claim.ClaimType, &claim.Status, &claim.CreatedByRunID, &claim.UpdatedByRunID, &chunkID, &relation, &note, &chunk.ID, &chunkDocID, &chunk.ChunkIndex, &chunk.Text, &page, &heading, &chunk.CharStart, &chunk.CharEnd, &chunk.ContentHash, &source.ID, &source.OriginalName, &source.MediaType, &source.SHA256, &source.OriginalPath, &source.ParsedPath, &source.Status, &source.ParserVersion, &sourceCreated, &source.ParseError); err != nil {
			return nil, err
		}
		claim.SectionID = section.String
		if index, ok := byID[claim.ID]; ok {
			if chunkID.String != "" {
				result[index].Evidence = append(result[index].Evidence, makeEvidence(claim.ID, chunk, source, relation.String, note.String, page, heading, sourceCreated))
			}
			continue
		}
		view := ClaimView{Claim: claim, Evidence: []EvidenceView{}}
		if chunkID.String != "" {
			view.Evidence = append(view.Evidence, makeEvidence(claim.ID, chunk, source, relation.String, note.String, page, heading, sourceCreated))
		}
		byID[claim.ID] = len(result)
		result = append(result, view)
	}
	return result, rows.Err()
}

func makeEvidence(claimID string, chunk domain.SourceChunk, source domain.SourceDocument, relation, note string, page sql.NullInt64, heading, sourceCreated sql.NullString) EvidenceView {
	chunk.DocumentID = source.ID
	if page.Valid {
		value := int(page.Int64)
		chunk.PageNumber = &value
	}
	_ = json.Unmarshal([]byte(heading.String), &chunk.HeadingPath)
	source.CreatedAt, _ = time.Parse(time.RFC3339Nano, sourceCreated.String)
	return EvidenceView{Evidence: domain.ClaimEvidence{ClaimID: claimID, SourceChunkID: chunk.ID, Relation: relation, Note: note}, Chunk: chunk, Source: source}
}

func (s Service) sections(ctx context.Context, pageID string, claims []ClaimView) ([]SectionView, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, page_id, heading, position, summary FROM wiki_sections WHERE page_id = ? AND knowledge_base_id = ? ORDER BY position, id`, pageID, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sections := make([]SectionView, 0)
	for rows.Next() {
		var section domain.WikiSection
		if err := rows.Scan(&section.ID, &section.PageID, &section.Heading, &section.Position, &section.Summary); err != nil {
			return nil, err
		}
		sections = append(sections, SectionView{Section: section})
	}
	if len(sections) == 0 {
		section := domain.WikiSection{ID: pageID + ":claims", PageID: pageID, Heading: "Claims", Position: 0, Summary: "Compiled claims and their source evidence."}
		return []SectionView{{Section: section, Claims: claims}}, nil
	}
	for i := range sections {
		for _, claim := range claims {
			if claim.Claim.SectionID == sections[i].Section.ID {
				sections[i].Claims = append(sections[i].Claims, claim)
			}
		}
	}
	return sections, rows.Err()
}

func (s Service) links(ctx context.Context, pageID string, outgoing bool) ([]LinkView, error) {
	condition := "l.source_page_id = ?"
	joinID := "l.target_page_id"
	if !outgoing {
		condition = "l.target_page_id = ?"
		joinID = "l.source_page_id"
	}
	rows, err := s.DB.QueryContext(ctx, fmt.Sprintf(`SELECT l.source_page_id, l.target_page_id, l.relation, l.created_by_run_id,
       p.id, p.slug, p.page_type, p.title, p.summary, p.status, p.current_revision, p.created_at, p.updated_at
		FROM wiki_links l JOIN wiki_pages p ON p.id = %s AND p.status = 'active' AND p.knowledge_base_id = l.knowledge_base_id JOIN wiki_pages sp ON sp.id = l.source_page_id AND sp.status = 'active' AND sp.knowledge_base_id = l.knowledge_base_id WHERE l.knowledge_base_id = ? AND %s ORDER BY p.slug`, joinID, condition), s.scope(ctx), pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]LinkView, 0)
	for rows.Next() {
		var link domain.WikiLink
		var page domain.WikiPage
		var created, updated string
		if err := rows.Scan(&link.SourcePageID, &link.TargetPageID, &link.Relation, &link.CreatedByRunID, &page.ID, &page.Slug, &page.PageType, &page.Title, &page.Summary, &page.Status, &page.CurrentRevision, &created, &updated); err != nil {
			return nil, err
		}
		page.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		page.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		result = append(result, LinkView{Link: link, Page: page})
	}
	return result, rows.Err()
}

func (s Service) lintProjection(ctx context.Context, issues *[]LintIssue) error {
	if s.DataRoot == "" {
		return nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT slug, page_type FROM wiki_pages WHERE status != 'active' AND knowledge_base_id = ? ORDER BY slug`, s.scope(ctx))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		var pageType domain.PageType
		if err := rows.Scan(&slug, &pageType); err != nil {
			return err
		}
		for _, path := range []string{filepath.Join(s.DataRoot, "wiki", slug+".md"), filepath.Join(s.DataRoot, "wiki", string(pageType)+"s", slug+".md")} {
			if _, err := os.Stat(path); err == nil {
				*issues = append(*issues, LintIssue{Code: "inactive_page_projection", Severity: "error", EntityID: slug, Message: fmt.Sprintf("non-active page still has a Markdown projection at %s", path)})
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}
	return rows.Err()
}

func (s Service) pageSources(ctx context.Context, pageID string) ([]SourceTrace, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT sc.document_id FROM wiki_claims c JOIN claim_evidence ce ON ce.claim_id = c.id AND ce.knowledge_base_id = c.knowledge_base_id JOIN source_chunks sc ON sc.id = ce.source_chunk_id AND sc.knowledge_base_id = c.knowledge_base_id WHERE c.page_id = ? AND c.knowledge_base_id = ? ORDER BY sc.document_id`, pageID, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	result := make([]SourceTrace, 0, len(ids))
	for _, id := range ids {
		trace, err := s.SourceTrace(ctx, id)
		if err != nil {
			return nil, err
		}
		filtered := trace.EvidenceHits[:0]
		for _, hit := range trace.EvidenceHits {
			var page string
			_ = s.DB.QueryRowContext(ctx, `SELECT page_id FROM wiki_claims WHERE id = ? AND knowledge_base_id = ?`, hit.Evidence.ClaimID, s.scope(ctx)).Scan(&page)
			if page == pageID {
				filtered = append(filtered, hit)
			}
		}
		trace.EvidenceHits = filtered
		result = append(result, trace)
	}
	return result, nil
}

func scanEvidenceView(row interface{ Scan(...any) error }, source domain.SourceDocument) (EvidenceView, error) {
	var evidence domain.ClaimEvidence
	var chunk domain.SourceChunk
	var page sql.NullInt64
	var heading string
	if err := row.Scan(&evidence.ClaimID, &evidence.SourceChunkID, &evidence.Relation, &evidence.Note, &chunk.ID, &chunk.DocumentID, &chunk.ChunkIndex, &chunk.Text, &page, &heading, &chunk.CharStart, &chunk.CharEnd, &chunk.ContentHash); err != nil {
		return EvidenceView{}, err
	}
	if page.Valid {
		value := int(page.Int64)
		chunk.PageNumber = &value
	}
	_ = json.Unmarshal([]byte(heading), &chunk.HeadingPath)
	return EvidenceView{Evidence: evidence, Chunk: chunk, Source: source}, nil
}

func diffSnapshots(current, previous map[string]any) RevisionDiff {
	claims := func(value map[string]any) map[string]string {
		result := map[string]string{}
		list, _ := value["claims"].([]any)
		for _, raw := range list {
			item, _ := raw.(map[string]any)
			id, _ := item["id"].(string)
			text, _ := item["text"].(string)
			status, _ := item["status"].(string)
			result[id] = text + "|" + status
		}
		return result
	}
	a, b := claims(current), claims(previous)
	diff := RevisionDiff{AddedClaims: []string{}, RemovedClaims: []string{}, Changed: []string{}}
	for id, value := range a {
		old, ok := b[id]
		if !ok {
			diff.AddedClaims = append(diff.AddedClaims, id)
		} else if old != value {
			diff.Changed = append(diff.Changed, id)
		}
	}
	for id := range b {
		if _, ok := a[id]; !ok {
			diff.RemovedClaims = append(diff.RemovedClaims, id)
		}
	}
	sort.Strings(diff.AddedClaims)
	sort.Strings(diff.RemovedClaims)
	sort.Strings(diff.Changed)
	return diff
}
