package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	if err := ensureSQLiteDirectory(dsn); err != nil {
		return nil, err
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	if err = database.PingContext(ctx); err != nil {
		database.Close()
		return nil, err
	}
	goose.SetBaseFS(migrationFS)
	if err = goose.SetDialect("sqlite3"); err != nil {
		database.Close()
		return nil, err
	}
	if err = goose.Up(database, "migrations"); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}

func ensureSQLiteDirectory(dsn string) error {
	if dsn == "" || dsn == ":memory:" {
		return nil
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse sqlite DSN: %w", err)
	}
	if parsed.Scheme != "file" || parsed.Path == "" || parsed.Path == ":memory:" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(parsed.Path), 0o755); err != nil {
		return fmt.Errorf("create sqlite directory: %w", err)
	}
	return nil
}
