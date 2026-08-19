-- +goose Up
ALTER TABLE messages ADD COLUMN retrieval_trace_id TEXT REFERENCES retrieval_traces(id);
ALTER TABLE messages ADD COLUMN standalone_query TEXT;
ALTER TABLE messages ADD COLUMN citations_json TEXT;
ALTER TABLE messages ADD COLUMN context_json TEXT;
CREATE INDEX IF NOT EXISTS idx_messages_conversation_created ON messages(conversation_id, created_at, id);

-- +goose Down
DROP INDEX IF EXISTS idx_messages_conversation_created;
ALTER TABLE messages DROP COLUMN context_json;
ALTER TABLE messages DROP COLUMN citations_json;
ALTER TABLE messages DROP COLUMN standalone_query;
ALTER TABLE messages DROP COLUMN retrieval_trace_id;
