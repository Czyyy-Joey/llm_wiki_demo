package retrieval

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/indexing"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	chromem "github.com/philippgille/chromem-go"
)

type Service struct {
	DB            *sql.DB
	Embedder      llm.EmbeddingClient
	TopK          int
	ContextBudget int
	IndexDir      string
}

type Candidate struct {
	PassageID      string   `json:"passage_id"`
	PageID         string   `json:"page_id"`
	Slug           string   `json:"slug"`
	Title          string   `json:"title"`
	PageType       string   `json:"page_type"`
	Text           string   `json:"text"`
	FTSScore       float64  `json:"fts_score"`
	VectorScore    float64  `json:"vector_score"`
	RRFScore       float64  `json:"rrf_score"`
	ExpansionScore float64  `json:"expansion_score"`
	FinalScore     float64  `json:"final_score"`
	Expanded       bool     `json:"expanded"`
	ExpansionFrom  []string `json:"expansion_from,omitempty"`
	Selected       bool     `json:"selected"`
	DiscardReason  string   `json:"discard_reason,omitempty"`
}

type ContextItem struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	PageID        string `json:"page_id,omitempty"`
	PassageID     string `json:"passage_id,omitempty"`
	SourceChunkID string `json:"source_chunk_id,omitempty"`
	Text          string `json:"text"`
	Citation      string `json:"citation"`
}

type Result struct {
	Query      string                `json:"query"`
	Candidates []Candidate           `json:"candidates"`
	Context    []ContextItem         `json:"context"`
	Trace      domain.RetrievalTrace `json:"trace"`
}

func (s Service) Search(ctx context.Context, query string) (Result, error) {
	normalized := indexing.Normalize(query)
	if normalized == "" {
		return Result{}, fmt.Errorf("query is required")
	}
	if s.TopK <= 0 {
		s.TopK = 10
	}
	if s.ContextBudget <= 0 {
		s.ContextBudget = 6000
	}
	if s.Embedder == nil {
		s.Embedder = llm.DeterministicEmbedding{}
	}
	fts, err := s.fts(ctx, normalized, s.TopK*2)
	if err != nil {
		return Result{}, err
	}
	vector, queryVector, err := s.vector(ctx, normalized, s.TopK*2)
	if err != nil {
		return Result{}, err
	}
	merged := fuse(fts, vector)
	mergedCandidates := make([]Candidate, 0, len(merged))
	for _, item := range merged {
		mergedCandidates = append(mergedCandidates, item)
	}
	sort.Slice(mergedCandidates, func(i, j int) bool {
		if mergedCandidates[i].RRFScore != mergedCandidates[j].RRFScore {
			return mergedCandidates[i].RRFScore > mergedCandidates[j].RRFScore
		}
		return mergedCandidates[i].PageID < mergedCandidates[j].PageID
	})
	seeds := make([]string, 0, len(mergedCandidates))
	seedCount := 0
	for _, item := range mergedCandidates {
		if seedCount >= s.TopK {
			break
		}
		seeds = append(seeds, item.PageID)
		seedCount++
	}
	expanded, err := s.expand(ctx, seeds)
	if err != nil {
		return Result{}, err
	}
	for _, candidate := range expanded {
		if existing, ok := merged[candidate.PageID]; ok {
			if candidate.ExpansionScore > existing.ExpansionScore {
				existing.ExpansionScore = candidate.ExpansionScore
				existing.Expanded = true
				existing.ExpansionFrom = candidate.ExpansionFrom
				merged[candidate.PageID] = existing
			}
			continue
		}
		candidate.FinalScore = candidate.ExpansionScore * 0.005
		merged[candidate.PageID] = candidate
	}
	if err := s.hydrateCandidates(ctx, merged); err != nil {
		return Result{}, err
	}
	candidates := make([]Candidate, 0, len(merged))
	for _, candidate := range merged {
		candidate.FinalScore = candidate.RRFScore + candidate.ExpansionScore*0.005
		if indexing.Normalize(candidate.Title) == normalized {
			candidate.FinalScore += 0.02
		}
		merged[candidate.PageID] = candidate
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].FinalScore != candidates[j].FinalScore {
			return candidates[i].FinalScore > candidates[j].FinalScore
		}
		return candidates[i].Slug < candidates[j].Slug
	})
	if len(candidates) > s.TopK {
		selected := append([]Candidate(nil), candidates[:s.TopK]...)
		// Keep one expanded page visible alongside the initial top-k seed so
		// link expansion can contribute context without hiding the best seed.
		for _, candidate := range candidates[s.TopK:] {
			if candidate.Expanded {
				selected = append(selected, candidate)
				break
			}
		}
		candidates = selected
	}
	contextItems, fallbackIDs, err := s.context(ctx, candidates, queryVector, s.ContextBudget)
	if err != nil {
		return Result{}, err
	}
	trace := domain.RetrievalTrace{ID: traceID(normalized), NormalizedQuery: normalized, FTSCandidates: ids(fts), VectorCandidates: ids(vector), RRFScore: map[string]float64{}, ExpandedPages: []string{}, FinalCandidates: []string{}, DroppedCandidates: []string{}, ContextIDs: []string{}, ContextBudget: s.ContextBudget, SourceFallback: fallbackIDs, Candidates: []domain.RetrievalCandidateTrace{}, Notes: []string{"fts and vector search executed", "wiki passages are the primary retrieval object"}}
	for _, item := range merged {
		trace.RRFScore[item.PageID] = item.RRFScore
	}
	for _, item := range expanded {
		trace.ExpandedPages = append(trace.ExpandedPages, item.PageID)
	}
	selectedPages := map[string]bool{}
	for i := range candidates {
		candidates[i].Selected = true
		selectedPages[candidates[i].PageID] = true
		trace.FinalCandidates = append(trace.FinalCandidates, candidates[i].PageID)
	}
	traceCandidates := make([]Candidate, 0, len(merged))
	for _, item := range merged {
		traceCandidates = append(traceCandidates, item)
	}
	sort.Slice(traceCandidates, func(i, j int) bool {
		if traceCandidates[i].FinalScore != traceCandidates[j].FinalScore {
			return traceCandidates[i].FinalScore > traceCandidates[j].FinalScore
		}
		return traceCandidates[i].PageID < traceCandidates[j].PageID
	})
	for _, item := range traceCandidates {
		selected := selectedPages[item.PageID]
		selectionReason := ""
		discardReason := ""
		if !selected {
			discardReason = "below final top-k"
			trace.DroppedCandidates = append(trace.DroppedCandidates, item.PageID)
		} else if item.Expanded && item.RRFScore == 0 {
			selectionReason = "selected by one-hop Wiki link expansion"
		} else {
			selectionReason = "selected by deterministic final score"
		}
		trace.Candidates = append(trace.Candidates, domain.RetrievalCandidateTrace{PageID: item.PageID, PassageID: item.PassageID, FTSScore: item.FTSScore, VectorScore: item.VectorScore, RRFScore: item.RRFScore, ExpansionScore: item.ExpansionScore, FinalScore: item.FinalScore, Expanded: item.Expanded, ExpansionFrom: item.ExpansionFrom, Selected: selected, SelectionReason: selectionReason, DiscardReason: discardReason})
	}
	for _, item := range contextItems {
		trace.ContextIDs = append(trace.ContextIDs, item.ID)
		trace.ContextUsed += contextCost(item.Text)
	}
	if len(candidates) == 0 {
		trace.Notes = append(trace.Notes, "wiki returned no candidates; source fallback considered")
	}
	if len(fallbackIDs) > 0 {
		trace.Notes = append(trace.Notes, "source chunks were used only as evidence fallback")
	}
	if err := s.saveTrace(ctx, trace); err != nil {
		return Result{}, err
	}
	return Result{Query: query, Candidates: candidates, Context: contextItems, Trace: trace}, nil
}

type ranked struct {
	ID     string
	PageID string
	Rank   int
	Score  float64
}

func (s Service) fts(ctx context.Context, query string, limit int) ([]ranked, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT f.passage_id, f.page_id, bm25(wiki_fts) FROM wiki_fts f JOIN wiki_pages p ON p.id = f.page_id AND p.status = 'active' WHERE wiki_fts MATCH ? ORDER BY bm25(wiki_fts) LIMIT ?`, ftsQuery(query), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ranked{}
	for rows.Next() {
		var item ranked
		if err := rows.Scan(&item.ID, &item.PageID, &item.Score); err != nil {
			return nil, err
		}
		item.Rank = len(result) + 1
		item.Score = 1 / (1 + maxFloat(0, item.Score))
		result = append(result, item)
	}
	if len(result) == 0 {
		rows.Close()
		fallback, fallbackErr := s.DB.QueryContext(ctx, `SELECT w.id, w.page_id, 0.5 FROM wiki_passages w JOIN wiki_pages p ON p.id = w.page_id AND p.status = 'active' WHERE lower(w.title || ' ' || w.text) LIKE ? ORDER BY w.page_id LIMIT ?`, "%"+strings.ToLower(query)+"%", limit)
		if fallbackErr != nil {
			return nil, fallbackErr
		}
		defer fallback.Close()
		for fallback.Next() {
			var item ranked
			if err := fallback.Scan(&item.ID, &item.PageID, &item.Score); err != nil {
				return nil, err
			}
			item.Rank = len(result) + 1
			result = append(result, item)
		}
	}
	return result, rows.Err()
}

func ftsQuery(query string) string {
	tokens := indexing.Tokenize(query)
	if len(tokens) == 0 {
		return `""`
	}
	quoted := make([]string, len(tokens))
	for i, token := range tokens {
		quoted[i] = `"` + strings.ReplaceAll(token, `"`, ` `) + `"`
	}
	return strings.Join(quoted, " OR ")
}

func (s Service) vector(ctx context.Context, query string, limit int) ([]ranked, []float32, error) {
	vectors, err := s.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, nil, err
	}
	if len(vectors) != 1 {
		return nil, nil, fmt.Errorf("embedding provider returned invalid query vector")
	}
	results, found, err := s.queryVectorIndex(ctx, indexing.WikiCollection, vectors[0], limit)
	if err != nil {
		return nil, nil, err
	}
	if found {
		active, err := s.activePageIDs(ctx)
		if err != nil {
			return nil, nil, err
		}
		result := make([]ranked, 0, len(results))
		for _, value := range results {
			pageID := value.Metadata["page_id"]
			if pageID == "" || !active[pageID] {
				continue
			}
			result = append(result, ranked{ID: value.ID, PageID: pageID, Score: float64(value.Similarity)})
		}
		return rankVectors(result, limit), vectors[0], nil
	}
	records, err := s.sqliteWikiVectors(ctx)
	if err != nil {
		return nil, nil, err
	}
	return rankStoredVectors(records, vectors[0], limit), vectors[0], nil
}

type storedVector struct {
	ID        string
	PageID    string
	Embedding []float32
}

func (s Service) sqliteWikiVectors(ctx context.Context) ([]storedVector, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT w.id, w.page_id, w.embedding_json FROM wiki_passages w JOIN wiki_pages p ON p.id = w.page_id AND p.status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []storedVector{}
	for rows.Next() {
		var item storedVector
		var raw string
		if err := rows.Scan(&item.ID, &item.PageID, &raw); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(raw), &item.Embedding) == nil {
			result = append(result, item)
		}
	}
	return result, rows.Err()
}

func rankStoredVectors(records []storedVector, query []float32, limit int) []ranked {
	result := make([]ranked, 0, len(records))
	for _, record := range records {
		result = append(result, ranked{ID: record.ID, PageID: record.PageID, Score: indexing.Cosine(query, record.Embedding)})
	}
	return rankVectors(result, limit)
}

func rankVectors(result []ranked, limit int) []ranked {
	sort.Slice(result, func(i, j int) bool {
		if result[i].Score != result[j].Score {
			return result[i].Score > result[j].Score
		}
		return result[i].ID < result[j].ID
	})
	if len(result) > limit {
		result = result[:limit]
	}
	for i := range result {
		result[i].Rank = i + 1
	}
	return result
}

func (s Service) queryVectorIndex(ctx context.Context, name string, query []float32, limit int) ([]chromem.Result, bool, error) {
	if strings.TrimSpace(s.IndexDir) == "" {
		return nil, false, nil
	}
	if _, err := os.Stat(s.IndexDir); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	vectorDB, err := chromem.NewPersistentDB(s.IndexDir, false)
	if err != nil {
		return nil, false, fmt.Errorf("open vector index: %w", err)
	}
	collection := vectorDB.GetCollection(name, nil)
	if collection == nil || collection.Count() == 0 {
		return nil, false, nil
	}
	if limit > collection.Count() {
		limit = collection.Count()
	}
	if name == indexing.WikiCollection {
		// Resolve equal-score ties before truncation so chromem's internal order
		// cannot choose a different top-k subset between identical queries.
		limit = collection.Count()
	}
	results, err := collection.QueryEmbedding(ctx, query, limit, nil, nil)
	if err != nil {
		return nil, true, fmt.Errorf("query %s vector collection: %w", name, err)
	}
	return results, true, nil
}

func (s Service) activePageIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM wiki_pages WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result[id] = true
	}
	return result, rows.Err()
}

func fuse(fts, vector []ranked) map[string]Candidate {
	result := map[string]Candidate{}
	for _, item := range fts {
		current := result[item.PageID]
		current.PageID = item.PageID
		current.PassageID = item.ID
		current.FTSScore = item.Score
		current.RRFScore += 1 / float64(60+item.Rank)
		result[item.PageID] = current
	}
	for _, item := range vector {
		current := result[item.PageID]
		current.PageID = item.PageID
		if current.PassageID == "" {
			current.PassageID = item.ID
		}
		current.VectorScore = item.Score
		current.RRFScore += 1 / float64(60+item.Rank)
		result[item.PageID] = current
	}
	return result
}

func (s Service) expand(ctx context.Context, pageIDs []string) ([]Candidate, error) {
	if len(pageIDs) == 0 {
		return nil, nil
	}
	marks := strings.TrimRight(strings.Repeat("?,", len(pageIDs)), ",")
	args := make([]any, len(pageIDs))
	for i := range pageIDs {
		args[i] = pageIDs[i]
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT l.source_page_id, l.target_page_id, p.id, target.slug, target.title, target.page_type, p.text FROM wiki_links l JOIN wiki_pages sp ON sp.id = l.source_page_id AND sp.status = 'active' JOIN wiki_pages tp ON tp.id = l.target_page_id AND tp.status = 'active' JOIN wiki_passages p ON p.page_id = CASE WHEN l.source_page_id IN (`+marks+`) THEN l.target_page_id ELSE l.source_page_id END JOIN wiki_pages target ON target.id = p.page_id AND target.status = 'active' WHERE l.relation != 'merged_into' AND (l.source_page_id IN (`+marks+`) OR l.target_page_id IN (`+marks+`))`, append(append(append([]any{}, args...), args...), args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	result := []Candidate{}
	for rows.Next() {
		var source, target string
		var item Candidate
		if err := rows.Scan(&source, &target, &item.PassageID, &item.Slug, &item.Title, &item.PageType, &item.Text); err != nil {
			return nil, err
		}
		if contains(pageIDs, source) {
			item.PageID = target
			item.ExpansionFrom = []string{source}
		} else {
			item.PageID = source
			item.ExpansionFrom = []string{target}
		}
		if seen[item.PageID] {
			continue
		}
		if contains(pageIDs, item.PageID) {
			continue
		}
		seen[item.PageID] = true
		item.Expanded = true
		item.ExpansionScore = 1
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s Service) hydrateCandidates(ctx context.Context, candidates map[string]Candidate) error {
	for pageID, candidate := range candidates {
		var slug, title, pageType, text string
		err := s.DB.QueryRowContext(ctx, `SELECT p.slug, p.title, p.page_type, w.text FROM wiki_pages p JOIN wiki_passages w ON w.page_id = p.id WHERE p.id = ? AND p.status = 'active' ORDER BY w.id LIMIT 1`, pageID).Scan(&slug, &title, &pageType, &text)
		if err != nil {
			if err == sql.ErrNoRows {
				delete(candidates, pageID)
				continue
			}
			return err
		}
		candidate.Slug, candidate.Title, candidate.PageType, candidate.Text = slug, title, pageType, text
		candidates[pageID] = candidate
	}
	return nil
}

func (s Service) context(ctx context.Context, candidates []Candidate, queryVector []float32, budget int) ([]ContextItem, []string, error) {
	result := []ContextItem{}
	fallbackIDs := []string{}
	used := 0
	addedSource := map[string]bool{}
	needsFallback := len(candidates) == 0
	for _, candidate := range candidates {
		text := candidate.Text
		cost := contextCost(text)
		if used+cost > budget {
			needsFallback = true
			continue
		}
		result = append(result, ContextItem{ID: candidate.PassageID, Kind: "wiki", PageID: candidate.PageID, PassageID: candidate.PassageID, Text: text, Citation: candidate.Slug})
		used += cost
		candidateEvidenceAdded := false
		rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT ce.source_chunk_id, sc.text FROM wiki_claims c JOIN claim_evidence ce ON ce.claim_id = c.id JOIN source_chunks sc ON sc.id = ce.source_chunk_id WHERE c.page_id = ? AND c.status IN ('active','disputed') ORDER BY ce.source_chunk_id`, candidate.PageID)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var id, evidence string
			if err := rows.Scan(&id, &evidence); err != nil {
				rows.Close()
				return nil, nil, err
			}
			cost = contextCost(evidence)
			if used+cost > budget {
				continue
			}
			result = append(result, ContextItem{ID: id, Kind: "source_evidence", PageID: candidate.PageID, SourceChunkID: id, Text: evidence, Citation: id})
			addedSource[id] = true
			candidateEvidenceAdded = true
			used += cost
		}
		rows.Close()
		if !candidateEvidenceAdded {
			needsFallback = true
		}
	}
	// Source vectors are consulted only when a selected Wiki page lacks usable
	// evidence (or when the Wiki returned no context at all).
	if needsFallback && len(queryVector) > 0 {
		fallback, err := s.sourceFallback(ctx, queryVector, 3)
		if err != nil {
			return nil, nil, err
		}
		for _, item := range fallback {
			if addedSource[item.ID] {
				continue
			}
			cost := contextCost(item.Text)
			if used+cost > budget {
				continue
			}
			result = append(result, ContextItem{ID: item.ID, Kind: "source_fallback", SourceChunkID: item.ID, Text: item.Text, Citation: item.ID})
			fallbackIDs = append(fallbackIDs, item.ID)
			used += cost
		}
	}
	return result, fallbackIDs, nil
}

// contextCost is a deterministic token estimate: CJK characters count as one
// token, while other text is approximated at four runes per token.
func contextCost(text string) int {
	tokens := 0
	latinRunes := 0
	flushLatin := func() {
		if latinRunes > 0 {
			tokens += (latinRunes + 3) / 4
			latinRunes = 0
		}
	}
	for _, value := range text {
		if unicode.Is(unicode.Han, value) {
			flushLatin()
			tokens++
			continue
		}
		latinRunes++
	}
	flushLatin()
	return tokens
}

type sourceMatch struct {
	ID, Text string
	Score    float64
}

func (s Service) sourceFallback(ctx context.Context, queryVector []float32, limit int) ([]sourceMatch, error) {
	type sourceVector struct {
		ID        string
		Embedding []float32
	}
	items := []sourceVector{}
	results, found, err := s.queryVectorIndex(ctx, indexing.SourceCollection, queryVector, limit)
	if err != nil {
		return nil, err
	}
	if found {
		matches := make([]sourceMatch, 0, len(results))
		for _, item := range results {
			matches = append(matches, sourceMatch{ID: item.ID, Text: item.Content, Score: float64(item.Similarity)})
		}
		return matches, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT source_chunk_id, embedding_json FROM source_chunk_embeddings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item sourceVector
		var raw string
		if err := rows.Scan(&item.ID, &raw); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(raw), &item.Embedding) == nil {
			items = append(items, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	matches := make([]sourceMatch, 0, len(items))
	for _, item := range items {
		matches = append(matches, sourceMatch{ID: item.ID, Score: indexing.Cosine(queryVector, item.Embedding)})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		return matches[i].ID < matches[j].ID
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	for i := range matches {
		if err := s.DB.QueryRowContext(ctx, `SELECT text FROM source_chunks WHERE id = ?`, matches[i].ID).Scan(&matches[i].Text); err != nil {
			return nil, err
		}
	}
	return matches, nil
}

func (s Service) saveTrace(ctx context.Context, trace domain.RetrievalTrace) error {
	raw, err := json.Marshal(trace)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO retrieval_traces (id, normalized_query, trace_json, created_at) VALUES (?, ?, ?, ?)`, trace.ID, trace.NormalizedQuery, string(raw), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
func ids(items []ranked) []string {
	result := make([]string, len(items))
	for i := range items {
		result[i] = items[i].PageID
	}
	return result
}
func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func traceID(query string) string {
	sum := sha256.Sum256([]byte(query + time.Now().UTC().Format(time.RFC3339Nano)))
	return "trace_" + hex.EncodeToString(sum[:])
}
