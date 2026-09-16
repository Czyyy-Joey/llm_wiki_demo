-- +goose Up
-- +goose NO TRANSACTION
-- Knowledge bases are single-user workspaces. Existing rows are assigned to
-- the stable default workspace so this migration is data preserving.
CREATE TABLE IF NOT EXISTS knowledge_bases (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
    language TEXT NOT NULL DEFAULT 'zh' CHECK (language IN ('zh', 'en')),
    llm_base_url TEXT,
    llm_api_key TEXT,
    llm_model TEXT,
    embedding_base_url TEXT,
    embedding_model TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
INSERT OR IGNORE INTO knowledge_bases (id, name, description, status, language, created_at, updated_at)
VALUES ('default', 'Default Knowledge Base', 'Migrated existing workspace', 'active', 'zh', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);

PRAGMA foreign_keys = OFF;
CREATE TABLE source_documents_new (
    id TEXT PRIMARY KEY,
    original_name TEXT NOT NULL,
    media_type TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    original_path TEXT NOT NULL,
    parsed_path TEXT,
    status TEXT NOT NULL,
    parser_version TEXT NOT NULL,
    created_at TEXT NOT NULL,
    parse_error TEXT,
    knowledge_base_id TEXT NOT NULL DEFAULT 'default' REFERENCES knowledge_bases(id),
    UNIQUE(knowledge_base_id, sha256)
);
INSERT INTO source_documents_new (id, original_name, media_type, sha256, original_path, parsed_path, status, parser_version, created_at, parse_error, knowledge_base_id)
SELECT id, original_name, media_type, sha256, original_path, parsed_path, status, parser_version, created_at, parse_error, 'default' FROM source_documents;
DROP TABLE source_documents;
ALTER TABLE source_documents_new RENAME TO source_documents;

ALTER TABLE source_chunks ADD COLUMN knowledge_base_id TEXT NOT NULL DEFAULT 'default';
CREATE TABLE wiki_pages_new (
    id TEXT PRIMARY KEY,
    slug TEXT NOT NULL,
    page_type TEXT NOT NULL,
    title TEXT NOT NULL,
    summary TEXT NOT NULL,
    status TEXT NOT NULL,
    current_revision INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    index_status TEXT NOT NULL DEFAULT 'clean',
    knowledge_base_id TEXT NOT NULL DEFAULT 'default' REFERENCES knowledge_bases(id),
    UNIQUE(knowledge_base_id, slug)
);
INSERT INTO wiki_pages_new (id, slug, page_type, title, summary, status, current_revision, created_at, updated_at, index_status, knowledge_base_id)
SELECT id, slug, page_type, title, summary, status, current_revision, created_at, updated_at, index_status, 'default' FROM wiki_pages;
DROP TABLE wiki_pages;
ALTER TABLE wiki_pages_new RENAME TO wiki_pages;

ALTER TABLE wiki_sections ADD COLUMN knowledge_base_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE compilation_runs ADD COLUMN knowledge_base_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE wiki_claims ADD COLUMN knowledge_base_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE claim_evidence ADD COLUMN knowledge_base_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE wiki_links ADD COLUMN knowledge_base_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE wiki_revisions ADD COLUMN knowledge_base_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE retrieval_traces ADD COLUMN knowledge_base_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE conversations ADD COLUMN knowledge_base_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE messages ADD COLUMN knowledge_base_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE wiki_passages ADD COLUMN knowledge_base_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE source_chunk_embeddings ADD COLUMN knowledge_base_id TEXT NOT NULL DEFAULT 'default';
PRAGMA foreign_keys = ON;

-- FTS is a disposable projection. Recreate it with an unindexed workspace
-- column so every MATCH query can be storage-scoped.
DROP TABLE IF EXISTS wiki_fts;
CREATE VIRTUAL TABLE wiki_fts USING fts5(
    knowledge_base_id UNINDEXED,
    passage_id UNINDEXED,
    page_id UNINDEXED,
    title,
    text,
    tokenize = 'unicode61 remove_diacritics 2'
);
UPDATE wiki_pages SET index_status = 'index_pending' WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_source_documents_kb_sha ON source_documents(knowledge_base_id, sha256);
CREATE INDEX IF NOT EXISTS idx_source_chunks_kb_document ON source_chunks(knowledge_base_id, document_id);
CREATE INDEX IF NOT EXISTS idx_wiki_pages_kb_slug ON wiki_pages(knowledge_base_id, slug);
CREATE INDEX IF NOT EXISTS idx_wiki_sections_kb_page ON wiki_sections(knowledge_base_id, page_id);
CREATE INDEX IF NOT EXISTS idx_compilation_runs_kb_created ON compilation_runs(knowledge_base_id, created_at);
CREATE INDEX IF NOT EXISTS idx_wiki_claims_kb_page ON wiki_claims(knowledge_base_id, page_id);
CREATE INDEX IF NOT EXISTS idx_claim_evidence_kb_claim ON claim_evidence(knowledge_base_id, claim_id);
CREATE INDEX IF NOT EXISTS idx_wiki_links_kb_source ON wiki_links(knowledge_base_id, source_page_id);
CREATE INDEX IF NOT EXISTS idx_wiki_revisions_kb_page ON wiki_revisions(knowledge_base_id, page_id);
CREATE INDEX IF NOT EXISTS idx_retrieval_traces_kb_created ON retrieval_traces(knowledge_base_id, created_at);
CREATE INDEX IF NOT EXISTS idx_conversations_kb_updated ON conversations(knowledge_base_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_messages_kb_conversation ON messages(knowledge_base_id, conversation_id);
CREATE INDEX IF NOT EXISTS idx_wiki_passages_kb_page ON wiki_passages(knowledge_base_id, page_id);
CREATE INDEX IF NOT EXISTS idx_source_embeddings_kb_chunk ON source_chunk_embeddings(knowledge_base_id, source_chunk_id);

-- The rebuilt source/page tables intentionally use scoped composite UNIQUE
-- constraints. Existing rows were assigned to the default workspace above;
-- later workspaces may safely reuse the same source hash or page slug.

-- +goose Down
DROP INDEX IF EXISTS idx_source_embeddings_kb_chunk;
DROP INDEX IF EXISTS idx_wiki_passages_kb_page;
DROP INDEX IF EXISTS idx_messages_kb_conversation;
DROP INDEX IF EXISTS idx_conversations_kb_updated;
DROP INDEX IF EXISTS idx_retrieval_traces_kb_created;
DROP INDEX IF EXISTS idx_wiki_revisions_kb_page;
DROP INDEX IF EXISTS idx_wiki_links_kb_source;
DROP INDEX IF EXISTS idx_claim_evidence_kb_claim;
DROP INDEX IF EXISTS idx_wiki_claims_kb_page;
DROP INDEX IF EXISTS idx_compilation_runs_kb_created;
DROP INDEX IF EXISTS idx_wiki_sections_kb_page;
DROP INDEX IF EXISTS idx_wiki_pages_kb_slug;
DROP INDEX IF EXISTS idx_source_chunks_kb_document;
DROP INDEX IF EXISTS idx_source_documents_kb_sha;
DROP TABLE IF EXISTS knowledge_bases;
