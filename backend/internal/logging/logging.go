package logging

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type contextKey struct{}

var requestIDKey contextKey
var initMu sync.Mutex

// Init configures the process logger. The returned function closes the log file.
// Logs intentionally use a text format so terminal output and app.log are easy
// to inspect during a local demo.
func Init(level, path string) (func(), error) {
	initMu.Lock()
	defer initMu.Unlock()
	if strings.TrimSpace(path) == "" {
		path = "../logs/app.log"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	handler := slog.NewTextHandler(io.MultiWriter(os.Stdout, file), &slog.HandlerOptions{Level: parseLevel(level)})
	slog.SetDefault(slog.New(handler))
	return func() { _ = file.Close() }, nil
}

func parseLevel(value string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(requestIDKey).(string)
	return value
}

func Logger(ctx context.Context) *slog.Logger {
	logger := slog.Default()
	if id := RequestID(ctx); id != "" {
		return logger.With("request_id", id)
	}
	return logger
}

// SafeSummary is suitable for client-facing errors and structured log fields.
// It removes line breaks, limits size, and strips common credential-bearing
// material without attempting to expose a provider response body.
func SafeSummary(err error) string {
	if err == nil {
		return ""
	}
	return sanitize(err.Error(), 240)
}

// SafeDetail retains enough provider and database context for server-side
// diagnosis while applying the same credential redaction as SafeSummary.
func SafeDetail(err error) string {
	if err == nil {
		return ""
	}
	return sanitize(err.Error(), 2000)
}

func sanitize(message string, limit int) string {
	value := strings.Join(strings.Fields(message), " ")
	for _, marker := range []string{"Authorization:", "Bearer ", "api_key", "api key", "apikey", "access_token", "access token", "secret", "prompt"} {
		if index := strings.Index(strings.ToLower(value), strings.ToLower(marker)); index >= 0 {
			value = strings.TrimSpace(value[:index]) + "[redacted]"
			break
		}
	}
	if len(value) > limit {
		value = value[:limit] + "..."
	}
	return value
}

func SafeBody(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	return SafeSummary(errors.New(string(data)))
}

func Duration(start time.Time) int64 { return time.Since(start).Milliseconds() }
