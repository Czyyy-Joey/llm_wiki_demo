package sources

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/knowledgebase"
	"github.com/joeychen/llm-wiki-demo/backend/internal/logging"
	"github.com/joeychen/llm-wiki-demo/backend/internal/parsers"
)

const (
	StatusParsed  = "parsed"
	StatusFailed  = "failed"
	StatusPending = "pending"
)

type Service struct {
	DB              *sql.DB
	DataRoot        string
	KnowledgeBaseID string
}

func (s Service) scope() string { return knowledgebase.NormalizeID(s.KnowledgeBaseID) }

type IngestInput struct {
	OriginalName string
	MediaType    string
	Data         []byte
}

type IngestResult struct {
	Document domain.SourceDocument
	Chunks   []domain.SourceChunk
	NoOp     bool
}

type ChunkDetail struct {
	Chunk  domain.SourceChunk    `json:"chunk"`
	Source domain.SourceDocument `json:"source"`
}

func (s Service) Ingest(ctx context.Context, input IngestInput) (IngestResult, error) {
	started := time.Now()
	logging.Logger(ctx).Info("source ingest started", "name", filepath.Base(input.OriginalName), "bytes", len(input.Data))
	if strings.TrimSpace(input.OriginalName) == "" {
		return IngestResult{}, fmt.Errorf("original_name is required")
	}
	if len(input.Data) == 0 {
		return IngestResult{}, fmt.Errorf("source file is empty")
	}
	hash := sha256.Sum256(input.Data)
	sha := hex.EncodeToString(hash[:])
	documentID := "src_" + sha
	if s.scope() != knowledgebase.DefaultID {
		documentID = "src_" + s.scope() + "_" + sha
	}

	var existing domain.SourceDocument
	err := scanDocument(s.DB.QueryRowContext(ctx, `SELECT id, knowledge_base_id, original_name, media_type, sha256, original_path, COALESCE(parsed_path, ''), status, parser_version, created_at, COALESCE(parse_error, '') FROM source_documents WHERE knowledge_base_id = ? AND sha256 = ?`, s.scope(), sha), &existing)
	if err == nil {
		existingChunks, chunksErr := s.Chunks(ctx, existing.ID)
		if chunksErr != nil {
			return IngestResult{}, fmt.Errorf("load duplicate source chunks: %w", chunksErr)
		}
		logging.Logger(ctx).Info("source ingest duplicate", "source_id", existing.ID, "sha256", sha, "chunks", len(existingChunks), "duration_ms", logging.Duration(started))
		return IngestResult{Document: existing, Chunks: existingChunks, NoOp: true}, nil
	}
	if err != sql.ErrNoRows {
		return IngestResult{}, fmt.Errorf("check duplicate source: %w", err)
	}

	originalDir := filepath.Join(s.DataRoot, "sources", s.scope(), "original")
	if err := os.MkdirAll(originalDir, 0o755); err != nil {
		return IngestResult{}, fmt.Errorf("create original source directory: %w", err)
	}
	originalPath := filepath.Join(originalDir, sha+"_"+safeName(input.OriginalName))
	if err := writeIfAbsent(originalPath, input.Data); err != nil {
		return IngestResult{}, err
	}

	chunks, parseErr := parsers.Parse(parsers.Input{Name: input.OriginalName, MediaType: input.MediaType, Data: input.Data})
	now := time.Now().UTC()
	document := domain.SourceDocument{ID: documentID, KnowledgeBaseID: s.scope(), OriginalName: input.OriginalName, MediaType: parsers.NormalizeMediaType(input.OriginalName, input.MediaType), SHA256: sha, OriginalPath: originalPath, ParserVersion: parsers.ParserVersion, CreatedAt: now}
	if document.MediaType == "" {
		document.MediaType = "application/octet-stream"
	}
	if parseErr != nil {
		document.Status = StatusFailed
		document.ParseError = parseErr.Error()
		_, err = s.DB.ExecContext(ctx, `INSERT INTO source_documents (id, knowledge_base_id, original_name, media_type, sha256, original_path, parsed_path, status, parser_version, parse_error, created_at) VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?)`, document.ID, s.scope(), document.OriginalName, document.MediaType, document.SHA256, document.OriginalPath, document.Status, document.ParserVersion, document.ParseError, document.CreatedAt.Format(time.RFC3339Nano))
		if err != nil {
			return IngestResult{}, fmt.Errorf("save failed source: %w", err)
		}
		logging.Logger(ctx).Error("source parse failed", "source_id", document.ID, "parser_version", document.ParserVersion, "status", StatusFailed, "duration_ms", logging.Duration(started), "error", logging.SafeSummary(parseErr))
		return IngestResult{Document: document}, nil
	}
	document.Status = StatusParsed
	parsedPath := filepath.Join(s.DataRoot, "sources", s.scope(), "parsed", document.ID+".json")
	document.ParsedPath = parsedPath
	result := make([]domain.SourceChunk, 0, len(chunks))
	for _, chunk := range chunks {
		chunkHash := sha256.Sum256([]byte(chunk.Text))
		contentHash := hex.EncodeToString(chunkHash[:])
		stable := fmt.Sprintf("%s|%s|%d|%d|%d|%s", document.SHA256, parsers.ParserVersion, chunk.Index, chunk.CharStart, chunk.CharEnd, contentHash)
		idHash := sha256.Sum256([]byte(stable))
		id := "chunk_" + hex.EncodeToString(idHash[:])
		if s.scope() != knowledgebase.DefaultID {
			id = "chunk_" + s.scope() + "_" + hex.EncodeToString(idHash[:])
		}
		result = append(result, domain.SourceChunk{ID: id, KnowledgeBaseID: s.scope(), DocumentID: document.ID, ChunkIndex: chunk.Index, Text: chunk.Text, PageNumber: chunk.PageNumber, HeadingPath: chunk.HeadingPath, CharStart: chunk.CharStart, CharEnd: chunk.CharEnd, ContentHash: contentHash})
	}
	parsedJSON, _ := json.Marshal(result)
	if err = os.MkdirAll(filepath.Dir(parsedPath), 0o755); err != nil {
		return IngestResult{}, fmt.Errorf("create parsed source directory: %w", err)
	}
	if err = os.WriteFile(parsedPath, parsedJSON, 0o644); err != nil {
		return IngestResult{}, fmt.Errorf("write parsed source: %w", err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		_ = os.Remove(parsedPath)
		return IngestResult{}, fmt.Errorf("begin source transaction: %w", err)
	}
	defer tx.Rollback()
	if err = insertDocument(ctx, tx, document); err != nil {
		_ = os.Remove(parsedPath)
		return IngestResult{}, err
	}
	for _, chunk := range result {
		headingJSON, _ := json.Marshal(chunk.HeadingPath)
		var page any
		if chunk.PageNumber != nil {
			page = *chunk.PageNumber
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO source_chunks (id, knowledge_base_id, document_id, chunk_index, text, page_number, heading_path, char_start, char_end, content_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, chunk.ID, s.scope(), document.ID, chunk.ChunkIndex, chunk.Text, page, string(headingJSON), chunk.CharStart, chunk.CharEnd, chunk.ContentHash); err != nil {
			_ = os.Remove(parsedPath)
			return IngestResult{}, fmt.Errorf("save source chunk: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		_ = os.Remove(parsedPath)
		return IngestResult{}, fmt.Errorf("commit source: %w", err)
	}
	logging.Logger(ctx).Info("source ingest completed", "source_id", document.ID, "status", document.Status, "parser_version", document.ParserVersion, "chunks", len(result), "duration_ms", logging.Duration(started))
	return IngestResult{Document: document, Chunks: result}, nil
}

func (s Service) List(ctx context.Context) ([]domain.SourceDocument, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, knowledge_base_id, original_name, media_type, sha256, original_path, COALESCE(parsed_path, ''), status, parser_version, created_at, COALESCE(parse_error, '') FROM source_documents WHERE knowledge_base_id = ? ORDER BY created_at DESC`, s.scope())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.SourceDocument
	for rows.Next() {
		var item domain.SourceDocument
		if err := scanDocument(rows, &item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s Service) Get(ctx context.Context, id string) (domain.SourceDocument, error) {
	var item domain.SourceDocument
	row := s.DB.QueryRowContext(ctx, `SELECT id, knowledge_base_id, original_name, media_type, sha256, original_path, COALESCE(parsed_path, ''), status, parser_version, created_at, COALESCE(parse_error, '') FROM source_documents WHERE id = ? AND knowledge_base_id = ?`, id, s.scope())
	if err := scanDocument(row, &item); err != nil {
		if err == sql.ErrNoRows {
			return item, fmt.Errorf("source not found")
		}
		return item, err
	}
	return item, nil
}

func (s Service) Chunks(ctx context.Context, documentID string) ([]domain.SourceChunk, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, knowledge_base_id, document_id, chunk_index, text, page_number, heading_path, char_start, char_end, content_hash FROM source_chunks WHERE document_id = ? AND knowledge_base_id = ? ORDER BY chunk_index`, documentID, s.scope())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.SourceChunk
	for rows.Next() {
		var item domain.SourceChunk
		var page sql.NullInt64
		var heading string
		if err := rows.Scan(&item.ID, &item.KnowledgeBaseID, &item.DocumentID, &item.ChunkIndex, &item.Text, &page, &heading, &item.CharStart, &item.CharEnd, &item.ContentHash); err != nil {
			return nil, err
		}
		if page.Valid {
			value := int(page.Int64)
			item.PageNumber = &value
		}
		_ = json.Unmarshal([]byte(heading), &item.HeadingPath)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s Service) Chunk(ctx context.Context, id string) (ChunkDetail, error) {
	var result ChunkDetail
	var page sql.NullInt64
	var heading, created string
	err := s.DB.QueryRowContext(ctx, `SELECT c.id, c.knowledge_base_id, c.document_id, c.chunk_index, c.text, c.page_number, c.heading_path, c.char_start, c.char_end, c.content_hash, d.id, d.knowledge_base_id, d.original_name, d.media_type, d.sha256, d.original_path, COALESCE(d.parsed_path, ''), d.status, d.parser_version, d.created_at, COALESCE(d.parse_error, '') FROM source_chunks c JOIN source_documents d ON d.id = c.document_id AND d.knowledge_base_id = c.knowledge_base_id WHERE c.id = ? AND c.knowledge_base_id = ?`, id, s.scope()).Scan(
		&result.Chunk.ID, &result.Chunk.KnowledgeBaseID, &result.Chunk.DocumentID, &result.Chunk.ChunkIndex, &result.Chunk.Text, &page, &heading, &result.Chunk.CharStart, &result.Chunk.CharEnd, &result.Chunk.ContentHash,
		&result.Source.ID, &result.Source.KnowledgeBaseID, &result.Source.OriginalName, &result.Source.MediaType, &result.Source.SHA256, &result.Source.OriginalPath, &result.Source.ParsedPath, &result.Source.Status, &result.Source.ParserVersion, &created, &result.Source.ParseError,
	)
	if err != nil {
		return result, err
	}
	if page.Valid {
		value := int(page.Int64)
		result.Chunk.PageNumber = &value
	}
	_ = json.Unmarshal([]byte(heading), &result.Chunk.HeadingPath)
	result.Source.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return result, err
}

func insertDocument(ctx context.Context, tx *sql.Tx, document domain.SourceDocument) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO source_documents (id, knowledge_base_id, original_name, media_type, sha256, original_path, parsed_path, status, parser_version, parse_error, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`, document.ID, document.KnowledgeBaseID, document.OriginalName, document.MediaType, document.SHA256, document.OriginalPath, document.ParsedPath, document.Status, document.ParserVersion, document.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("save source document: %w", err)
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanDocument(row scanner, item *domain.SourceDocument) error {
	var created string
	if err := row.Scan(&item.ID, &item.KnowledgeBaseID, &item.OriginalName, &item.MediaType, &item.SHA256, &item.OriginalPath, &item.ParsedPath, &item.Status, &item.ParserVersion, &created, &item.ParseError); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return err
	}
	item.CreatedAt = parsed
	return nil
}
func writeIfAbsent(path string, data []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("preserve original source: %w", err)
	}
	return nil
}
func safeName(name string) string {
	name = filepath.Base(name)
	name = strings.ReplaceAll(name, "..", "_")
	return name
}
