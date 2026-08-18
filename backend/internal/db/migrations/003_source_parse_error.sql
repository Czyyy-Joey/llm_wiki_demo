-- +goose Up
ALTER TABLE source_documents ADD COLUMN parse_error TEXT;

-- +goose Down
-- SQLite does not support dropping columns on all supported versions.
