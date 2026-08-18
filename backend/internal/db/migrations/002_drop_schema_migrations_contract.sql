-- +goose Up
DROP TABLE IF EXISTS schema_migrations_contract;

-- +goose Down
CREATE TABLE IF NOT EXISTS schema_migrations_contract (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL);
