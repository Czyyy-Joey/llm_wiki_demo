-- +goose Up
ALTER TABLE wiki_pages ADD COLUMN index_status TEXT NOT NULL DEFAULT 'clean';
CREATE TABLE IF NOT EXISTS wiki_passages (
    id TEXT PRIMARY KEY,
    page_id TEXT NOT NULL REFERENCES wiki_pages(id),
    section_id TEXT,
    title TEXT NOT NULL,
    heading_path TEXT NOT NULL,
    text TEXT NOT NULL,
    page_type TEXT NOT NULL,
    revision INTEGER NOT NULL,
    content_hash TEXT NOT NULL,
    embedding_json TEXT NOT NULL,
    embedding_model TEXT NOT NULL,
    indexed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_wiki_passages_page ON wiki_passages(page_id);
CREATE TABLE IF NOT EXISTS source_chunk_embeddings (
    source_chunk_id TEXT PRIMARY KEY REFERENCES source_chunks(id),
    content_hash TEXT NOT NULL,
    embedding_json TEXT NOT NULL,
    embedding_model TEXT NOT NULL,
    indexed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_source_chunk_embeddings_hash ON source_chunk_embeddings(content_hash);
CREATE VIRTUAL TABLE IF NOT EXISTS wiki_fts USING fts5(
    passage_id UNINDEXED,
    page_id UNINDEXED,
    title,
    text,
    tokenize = 'unicode61 remove_diacritics 2'
);

-- +goose Down
DROP TABLE IF EXISTS wiki_fts;
DROP INDEX IF EXISTS idx_source_chunk_embeddings_hash;
DROP TABLE IF EXISTS source_chunk_embeddings;
DROP INDEX IF EXISTS idx_wiki_passages_page;
DROP TABLE IF EXISTS wiki_passages;
