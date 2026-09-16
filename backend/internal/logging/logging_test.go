package logging

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeSummaryRedactsCredentialsAndLargeBodies(t *testing.T) {
	message := "provider returned invalid API key secret-value prompt: full prompt contents"
	summary := SafeSummary(testError(message))
	if strings.Contains(summary, "secret-value") || strings.Contains(summary, "full prompt contents") {
		t.Fatalf("sensitive content leaked: %q", summary)
	}
	if len(summary) > 240 {
		t.Fatalf("summary length = %d", len(summary))
	}
}

func TestSafeDetailRetainsMoreDiagnosticContext(t *testing.T) {
	message := strings.Repeat("diagnostic ", 40)
	if len(SafeDetail(testError(message))) <= len(SafeSummary(testError(message))) {
		t.Fatal("SafeDetail should retain more context than SafeSummary")
	}
}

func TestInitWritesTerminalLoggerToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "app.log")
	closeLog, err := Init("debug", path)
	if err != nil {
		t.Fatal(err)
	}
	Logger(WithRequestID(context.Background(), "req-log-test")).Info("observable test event", "items", 2)
	closeLog()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)
	if !strings.Contains(contents, "observable test event") || !strings.Contains(contents, "req-log-test") {
		t.Fatalf("log file = %q", contents)
	}
}

type testError string

func (e testError) Error() string { return string(e) }
