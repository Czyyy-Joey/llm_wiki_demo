package indexing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/joeychen/llm-wiki-demo/backend/internal/knowledgebase"
	"github.com/joeychen/llm-wiki-demo/backend/internal/llm"
	"github.com/joeychen/llm-wiki-demo/backend/internal/logging"
	chromem "github.com/philippgille/chromem-go"
)

const (
	WikiEmbeddingModel = "deterministic-wiki-v1"
	DefaultBatchSize   = 32
	WikiCollection     = "wiki_passages"
	SourceCollection   = "source_chunks"
)

func ProviderIdentity(endpoint, model string) string {
	return strings.TrimRight(strings.TrimSpace(endpoint), "/") + "|" + strings.TrimSpace(model)
}

type Service struct {
	DB              *sql.DB
	Embedder        llm.EmbeddingClient
	Model           string
	ProviderID      string
	BatchSize       int
	IndexDir        string
	KnowledgeBaseID string
}

func (s Service) scope(ctx context.Context) string {
	return knowledgebase.Scope(ctx, s.KnowledgeBaseID)
}

type Passage struct {
	ID          string
	PageID      string
	Title       string
	HeadingPath string
	Text        string
	PageType    string
	Revision    int
	ContentHash string
	Embedding   []float32
}

type IndexResult struct {
	WikiPassages   int `json:"wiki_passages"`
	SourceChunks   int `json:"source_chunks"`
	EmbeddingsMade int `json:"embeddings_made"`
}

func (s Service) Reindex(ctx context.Context) (IndexResult, error) {
	started := time.Now()
	logging.Logger(ctx).Info("index reindex started", "provider_id", s.ProviderID, "model", s.Model)
	if s.DB == nil {
		return IndexResult{}, fmt.Errorf("index database is nil")
	}
	if s.Embedder == nil {
		s.Embedder = llm.DeterministicEmbedding{}
	}
	if s.BatchSize <= 0 {
		s.BatchSize = DefaultBatchSize
	}
	if s.Model == "" {
		s.Model = WikiEmbeddingModel
	}
	if s.ProviderID == "" {
		s.ProviderID = s.Model
	}
	passages, err := s.loadPassages(ctx)
	if err != nil {
		return IndexResult{}, err
	}
	chunks, err := s.loadChunks(ctx)
	if err != nil {
		return IndexResult{}, err
	}
	made := 0
	passageVectors, madeNow, err := s.fillEmbeddings(ctx, passageInputs(passages), false)
	if err != nil {
		return IndexResult{}, err
	}
	for i := range passages {
		passages[i].Embedding = passageVectors[passages[i].ID]
	}
	made += madeNow
	chunkVectors, madeNow, err := s.fillEmbeddings(ctx, chunkInputs(chunks), true)
	if err != nil {
		return IndexResult{}, err
	}
	for i := range chunks {
		chunks[i].Embedding = chunkVectors[chunks[i].ID]
	}
	made += madeNow
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return IndexResult{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM wiki_fts WHERE knowledge_base_id = ?`, s.scope(ctx)); err != nil {
		return IndexResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM wiki_passages WHERE knowledge_base_id = ?`, s.scope(ctx)); err != nil {
		return IndexResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM source_chunk_embeddings WHERE knowledge_base_id = ?`, s.scope(ctx)); err != nil {
		return IndexResult{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, passage := range passages {
		encoded, _ := json.Marshal(passage.Embedding)
		if _, err = tx.ExecContext(ctx, `INSERT INTO wiki_passages (id, knowledge_base_id, page_id, section_id, title, heading_path, text, page_type, revision, content_hash, embedding_json, embedding_model, indexed_at) VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, passage.ID, s.scope(ctx), passage.PageID, passage.Title, passage.HeadingPath, passage.Text, passage.PageType, passage.Revision, passage.ContentHash, string(encoded), s.ProviderID, now); err != nil {
			return IndexResult{}, err
		}
		ftsTitle := strings.Join(Tokenize(passage.Title), " ")
		ftsText := strings.Join(Tokenize(passage.Text), " ")
		if _, err = tx.ExecContext(ctx, `INSERT INTO wiki_fts (knowledge_base_id, passage_id, page_id, title, text) VALUES (?, ?, ?, ?, ?)`, s.scope(ctx), passage.ID, passage.PageID, ftsTitle, ftsText); err != nil {
			return IndexResult{}, err
		}
	}
	for _, chunk := range chunks {
		encoded, _ := json.Marshal(chunk.Embedding)
		if _, err = tx.ExecContext(ctx, `INSERT INTO source_chunk_embeddings (source_chunk_id, knowledge_base_id, content_hash, embedding_json, embedding_model, indexed_at) VALUES (?, ?, ?, ?, ?, ?)`, chunk.ID, s.scope(ctx), chunk.ContentHash, string(encoded), s.ProviderID, now); err != nil {
			return IndexResult{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE wiki_pages SET index_status = 'index_pending' WHERE status = 'active' AND knowledge_base_id = ?`, s.scope(ctx)); err != nil {
		return IndexResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return IndexResult{}, err
	}
	if err := s.writeVectorIndexes(ctx, passages, chunks); err != nil {
		logging.Logger(ctx).Error("index vector write failed", "wiki_passages", len(passages), "source_chunks", len(chunks), "duration_ms", logging.Duration(started), "error", logging.SafeSummary(err))
		return IndexResult{}, err
	}
	if _, err = s.DB.ExecContext(ctx, `UPDATE wiki_pages SET index_status = 'clean' WHERE status = 'active' AND knowledge_base_id = ?`, s.scope(ctx)); err != nil {
		return IndexResult{}, err
	}
	result := IndexResult{WikiPassages: len(passages), SourceChunks: len(chunks), EmbeddingsMade: made}
	logging.Logger(ctx).Info("index reindex completed", "wiki_passages", result.WikiPassages, "source_chunks", result.SourceChunks, "embeddings_made", result.EmbeddingsMade, "duration_ms", logging.Duration(started))
	return result, nil
}

// writeVectorIndexes builds chromem-go in a staging directory and swaps it into
// place. SQLite remains canonical, so the entire directory is disposable.
func (s Service) writeVectorIndexes(ctx context.Context, passages []Passage, chunks []SourceChunk) error {
	if strings.TrimSpace(s.IndexDir) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.IndexDir), 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(s.IndexDir), ".indexes-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	vectorDB, err := chromem.NewPersistentDB(staging, false)
	if err != nil {
		return fmt.Errorf("open staged vector index: %w", err)
	}
	wiki, err := vectorDB.GetOrCreateCollection(WikiCollection, map[string]string{"model": s.Model, "provider_id": s.ProviderID, "kind": "wiki"}, nil)
	if err != nil {
		return fmt.Errorf("create Wiki vector collection: %w", err)
	}
	wikiDocs := make([]chromem.Document, 0, len(passages))
	for _, passage := range passages {
		wikiDocs = append(wikiDocs, chromem.Document{
			ID:        passage.ID,
			Metadata:  map[string]string{"page_id": passage.PageID, "content_hash": passage.ContentHash, "status": "active"},
			Embedding: passage.Embedding,
			Content:   passage.Title + "\n" + passage.Text,
		})
	}
	if len(wikiDocs) > 0 {
		if err := wiki.AddDocuments(ctx, wikiDocs, 1); err != nil {
			return fmt.Errorf("write Wiki vector collection: %w", err)
		}
	}
	source, err := vectorDB.GetOrCreateCollection(SourceCollection, map[string]string{"model": s.Model, "provider_id": s.ProviderID, "kind": "source"}, nil)
	if err != nil {
		return fmt.Errorf("create Source vector collection: %w", err)
	}
	sourceDocs := make([]chromem.Document, 0, len(chunks))
	for _, chunk := range chunks {
		sourceDocs = append(sourceDocs, chromem.Document{
			ID:        chunk.ID,
			Metadata:  map[string]string{"content_hash": chunk.ContentHash},
			Embedding: chunk.Embedding,
			Content:   chunk.Text,
		})
	}
	if len(sourceDocs) > 0 {
		if err := source.AddDocuments(ctx, sourceDocs, 1); err != nil {
			return fmt.Errorf("write Source vector collection: %w", err)
		}
	}
	backup := s.IndexDir + ".backup"
	_ = os.RemoveAll(backup)
	if _, err := os.Stat(s.IndexDir); err == nil {
		if err := os.Rename(s.IndexDir, backup); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, s.IndexDir); err != nil {
		if _, statErr := os.Stat(backup); statErr == nil {
			_ = os.Rename(backup, s.IndexDir)
		}
		return err
	}
	return os.RemoveAll(backup)
}

type embeddable struct {
	ID          string
	Text        string
	ContentHash string
	Embedding   []float32
}

func (s Service) fillEmbeddings(ctx context.Context, work []embeddable, source bool) (map[string][]float32, int, error) {
	result := make(map[string][]float32, len(work))
	made := 0
	for start := 0; start < len(work); start += s.BatchSize {
		end := start + s.BatchSize
		if end > len(work) {
			end = len(work)
		}
		inputs := make([]string, 0, end-start)
		indexes := make([]int, 0, end-start)
		for i := start; i < end; i++ {
			model := s.ProviderID
			var hash, oldModel string
			var oldJSON sql.NullString
			table := "wiki_passages"
			idColumn := "id"
			if source {
				table = "source_chunk_embeddings"
				idColumn = "source_chunk_id"
			}
			_ = s.DB.QueryRow(`SELECT content_hash, embedding_model, embedding_json FROM `+table+` WHERE `+idColumn+` = ? AND knowledge_base_id = ?`, work[i].ID, s.scope(ctx)).Scan(&hash, &oldModel, &oldJSON)
			if hash == work[i].ContentHash && oldModel == model && oldJSON.Valid {
				_ = json.Unmarshal([]byte(oldJSON.String), &work[i].Embedding)
				continue
			}
			inputs = append(inputs, work[i].Text)
			indexes = append(indexes, i)
		}
		if len(inputs) == 0 {
			continue
		}
		vectors, err := s.Embedder.Embed(ctx, inputs)
		if err != nil {
			return result, made, err
		}
		if len(vectors) != len(inputs) {
			return result, made, fmt.Errorf("embedding provider returned %d vectors for %d inputs", len(vectors), len(inputs))
		}
		for i, vector := range vectors {
			if len(vector) == 0 {
				return result, made, fmt.Errorf("embedding provider returned empty vector")
			}
			work[indexes[i]].Embedding = vector
			result[work[indexes[i]].ID] = vector
			made++
		}
	}
	for _, item := range work {
		if _, ok := result[item.ID]; !ok {
			result[item.ID] = item.Embedding
		}
	}
	return result, made, nil
}

func passageInputs(values []Passage) []embeddable {
	result := make([]embeddable, 0, len(values))
	for _, value := range values {
		result = append(result, embeddable{ID: value.ID, Text: value.Title + "\n" + value.Text, ContentHash: value.ContentHash, Embedding: value.Embedding})
	}
	return result
}

func chunkInputs(values []SourceChunk) []embeddable {
	result := make([]embeddable, 0, len(values))
	for _, value := range values {
		result = append(result, embeddable{ID: value.ID, Text: value.Text, ContentHash: value.ContentHash, Embedding: value.Embedding})
	}
	return result
}

// Embedding is kept local to indexing so a fresh clone has a useful vector path
// without credentials. It hashes words and CJK bigrams into a fixed vector.
type SourceChunk struct {
	ID          string
	Text        string
	ContentHash string
	Embedding   []float32
}

func (s Service) loadPassages(ctx context.Context) ([]Passage, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT p.id, p.slug, p.title, p.summary, p.page_type, p.current_revision,
		COALESCE((SELECT GROUP_CONCAT(text, char(10)) FROM (SELECT c.text FROM wiki_claims c WHERE c.page_id = p.id AND c.knowledge_base_id = p.knowledge_base_id AND c.status IN ('active', 'disputed') ORDER BY c.id)), '')
		FROM wiki_pages p WHERE p.status = 'active' AND p.knowledge_base_id = ? ORDER BY p.slug`, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Passage, 0)
	for rows.Next() {
		var p Passage
		var slug, summary, claims string
		if err := rows.Scan(&p.PageID, &slug, &p.Title, &summary, &p.PageType, &p.Revision, &claims); err != nil {
			return nil, err
		}
		// A page has one stable passage in the demo. Revision changes the
		// content, not the identity of the indexed passage.
		p.ID = "passage_" + stableHash(s.scope(ctx)+":"+p.PageID+":page")
		p.HeadingPath = p.Title
		p.Text = strings.TrimSpace(strings.Join([]string{summary, claims}, "\n"))
		p.ContentHash = stableHash(p.Title + "\n" + p.Text + "\n" + p.PageType)
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s Service) loadChunks(ctx context.Context) ([]SourceChunk, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, text, content_hash FROM source_chunks WHERE knowledge_base_id = ? ORDER BY id`, s.scope(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]SourceChunk, 0)
	for rows.Next() {
		var c SourceChunk
		if err := rows.Scan(&c.ID, &c.Text, &c.ContentHash); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func stableHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, aa, bb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		aa += float64(a[i]) * float64(a[i])
		bb += float64(b[i]) * float64(b[i])
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return dot / (math.Sqrt(aa) * math.Sqrt(bb))
}

func Normalize(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func Tokenize(value string) []string {
	value = strings.ToLower(value)
	var result []string
	var latin []rune
	flush := func() {
		if len(latin) >= 2 {
			result = append(result, string(latin))
		}
		latin = nil
	}
	var han []rune
	flushHan := func() {
		if len(han) == 1 {
			result = append(result, string(han[0]))
		} else {
			for i := 0; i+1 < len(han); i++ {
				result = append(result, string(han[i:i+2]))
			}
		}
		han = nil
	}
	for _, r := range value {
		if unicode.Is(unicode.Han, r) {
			flush()
			han = append(han, r)
			continue
		}
		flushHan()
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			latin = append(latin, r)
		} else {
			flush()
		}
	}
	flush()
	flushHan()
	return result
}
