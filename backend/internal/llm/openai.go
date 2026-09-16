package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
	"github.com/joeychen/llm-wiki-demo/backend/internal/logging"
)

type OpenAICompatible struct {
	Endpoint  string
	APIKey    string
	Model     string
	PromptDir string
	HTTP      *http.Client
}

type OllamaEmbedding struct {
	Endpoint string
	Model    string
	HTTP     *http.Client
}

func NewOllamaEmbedding(endpoint, model string) *OllamaEmbedding {
	return &OllamaEmbedding{Endpoint: strings.TrimRight(endpoint, "/"), Model: model, HTTP: http.DefaultClient}
}

func (c *OllamaEmbedding) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	started := time.Now()
	if c == nil || c.Endpoint == "" || c.Model == "" {
		return nil, fmt.Errorf("ollama embedding provider is not configured")
	}
	body, err := json.Marshal(map[string]any{"model": c.Model, "input": inputs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		logging.Logger(ctx).Error("ollama embedding request", "endpoint", c.Endpoint+"/api/embed", "model", c.Model, "status", 0, "duration_ms", logging.Duration(started), "error", logging.SafeSummary(err))
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		logging.Logger(ctx).Error("ollama embedding response read failed", "endpoint", c.Endpoint+"/api/embed", "model", c.Model, "status", resp.StatusCode, "duration_ms", logging.Duration(started), "error", logging.SafeSummary(err))
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = fmt.Errorf("embedding HTTP %d: %s", resp.StatusCode, logging.SafeBody(data))
		logging.Logger(ctx).Error("ollama embedding request failed", "endpoint", c.Endpoint+"/api/embed", "model", c.Model, "status", resp.StatusCode, "duration_ms", logging.Duration(started), "error", logging.SafeSummary(err))
		return nil, err
	}
	var decoded struct {
		Embeddings [][]float32 `json:"embeddings"`
		Error      *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		logging.Logger(ctx).Error("ollama embedding decode failed", "endpoint", c.Endpoint+"/api/embed", "model", c.Model, "status", resp.StatusCode, "duration_ms", logging.Duration(started), "error", logging.SafeSummary(err))
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}
	if decoded.Error != nil {
		err = fmt.Errorf("embedding provider: %s", decoded.Error.Message)
		logging.Logger(ctx).Error("ollama embedding provider error", "endpoint", c.Endpoint+"/api/embed", "model", c.Model, "status", resp.StatusCode, "duration_ms", logging.Duration(started), "error", logging.SafeSummary(err))
		return nil, err
	}
	if len(decoded.Embeddings) != len(inputs) {
		err = fmt.Errorf("ollama embedding response returned %d vectors for %d inputs", len(decoded.Embeddings), len(inputs))
		logging.Logger(ctx).Error("ollama embedding batch mismatch", "endpoint", c.Endpoint+"/api/embed", "model", c.Model, "status", resp.StatusCode, "duration_ms", logging.Duration(started), "inputs", len(inputs), "error", logging.SafeSummary(err))
		return nil, err
	}
	for i, vector := range decoded.Embeddings {
		if len(vector) == 0 {
			err = fmt.Errorf("ollama embedding response missing vector %d", i)
			logging.Logger(ctx).Error("ollama embedding vector missing", "endpoint", c.Endpoint+"/api/embed", "model", c.Model, "status", resp.StatusCode, "duration_ms", logging.Duration(started), "inputs", len(inputs), "error", logging.SafeSummary(err))
			return nil, err
		}
	}
	logging.Logger(ctx).Info("ollama embedding request", "endpoint", c.Endpoint+"/api/embed", "model", c.Model, "status", resp.StatusCode, "duration_ms", logging.Duration(started), "inputs", len(inputs), "vectors", len(decoded.Embeddings), "dimensions", len(decoded.Embeddings[0]))
	return decoded.Embeddings, nil
}

func NewOpenAICompatible(endpoint, apiKey, model, promptDir string) *OpenAICompatible {
	return &OpenAICompatible{Endpoint: strings.TrimRight(endpoint, "/"), APIKey: apiKey, Model: model, PromptDir: promptDir, HTTP: http.DefaultClient}
}

func (c *OpenAICompatible) Name() string { return c.Model }

func (c *OpenAICompatible) Analyze(ctx context.Context, input AnalyzeInput) (json.RawMessage, error) {
	chunks, _ := json.Marshal(input.Chunks)
	document, _ := json.Marshal(input.Document)
	schema, _ := json.Marshal(SchemaFor[domain.SourceAnalysis]())
	prompt := LanguageInstruction(ctx) + "\n\n" + c.prompt("analyze.txt", defaultAnalyzePrompt)
	return c.complete(ctx, prompt, fmt.Sprintf("Document:\n%s\n\nSource chunks:\n%s", document, chunks), "source_analysis", schema)
}

func (c *OpenAICompatible) Plan(ctx context.Context, input PlanInput) (json.RawMessage, error) {
	schema, _ := json.Marshal(SchemaFor[domain.CompilationPlan]())
	prompt := LanguageInstruction(ctx) + "\n\n" + c.prompt("plan.txt", defaultPlanPrompt)
	return c.complete(ctx, prompt, fmt.Sprintf("Document ID: %s\n\nAnalysis:\n%s\n\nCandidates:\n%s", input.DocumentID, input.Analysis, input.Candidates), "compilation_plan", schema)
}

func (c *OpenAICompatible) Rewrite(ctx context.Context, question string, history []ChatTurn) (json.RawMessage, error) {
	schema, _ := json.Marshal(SchemaFor[domain.StandaloneQuery]())
	historyJSON, _ := json.Marshal(history)
	prompt := LanguageInstruction(ctx) + "\n\n" + c.prompt("rewrite.txt", defaultRewritePrompt)
	return c.complete(ctx, prompt, fmt.Sprintf("Recent history:\n%s\n\nFollow-up question:\n%s", historyJSON, question), "standalone_query", schema)
}

func (c *OpenAICompatible) Answer(ctx context.Context, question string, contextItems []AnswerContext) (json.RawMessage, error) {
	schema, _ := json.Marshal(SchemaFor[domain.GeneratedAnswer]())
	contextJSON, _ := json.Marshal(contextItems)
	prompt := LanguageInstruction(ctx) + "\n\n" + c.prompt("answer.txt", defaultAnswerPrompt)
	return c.complete(ctx, prompt, fmt.Sprintf("Question:\n%s\n\nAllowed context:\n%s", question, contextJSON), "grounded_answer", schema)
}

func (c *OpenAICompatible) prompt(name, fallback string) string {
	if c.PromptDir != "" {
		if data, err := os.ReadFile(filepath.Join(c.PromptDir, name)); err == nil && strings.TrimSpace(string(data)) != "" {
			return string(data)
		}
	}
	return fallback
}

type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []chatMessage  `json:"messages"`
	ResponseFormat responseFormat `json:"response_format"`
}
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type responseFormat struct {
	Type       string          `json:"type"`
	JSONSchema *responseSchema `json:"json_schema,omitempty"`
}
type responseSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}
type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *OpenAICompatible) complete(ctx context.Context, system, user, name string, schema json.RawMessage) (json.RawMessage, error) {
	if c == nil || c.Endpoint == "" || c.APIKey == "" || c.Model == "" {
		return nil, fmt.Errorf("LLM provider is not configured")
	}
	request := chatRequest{Model: c.Model, Messages: []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}}, ResponseFormat: responseFormat{Type: "json_schema", JSONSchema: &responseSchema{Name: name, Strict: true, Schema: schema}}}
	status, data, err := c.sendChat(ctx, request)
	if err != nil {
		return nil, err
	}
	if (status == http.StatusBadRequest || status == http.StatusUnprocessableEntity) && structuredOutputUnsupported(data) {
		request.Messages[0].Content += "\n\nReturn one JSON object that strictly matches this JSON Schema:\n" + string(schema)
		request.ResponseFormat = responseFormat{Type: "json_object"}
		status, data, err = c.sendChat(ctx, request)
		if err != nil {
			return nil, err
		}
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("LLM HTTP %d: %s", status, strings.TrimSpace(string(data)))
	}
	var result chatResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode LLM response: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("LLM error: %s", result.Error.Message)
	}
	if len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "" {
		return nil, fmt.Errorf("LLM returned no structured content")
	}
	return json.RawMessage(result.Choices[0].Message.Content), nil
}

func (c *OpenAICompatible) sendChat(ctx context.Context, request chatRequest) (int, []byte, error) {
	started := time.Now()
	body, err := json.Marshal(request)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		logging.Logger(ctx).Error("oneapi llm request", "endpoint", c.Endpoint+"/chat/completions", "model", c.Model, "status", 0, "duration_ms", logging.Duration(started), "error", logging.SafeSummary(err))
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		logging.Logger(ctx).Error("oneapi llm response read failed", "endpoint", c.Endpoint+"/chat/completions", "model", c.Model, "status", resp.StatusCode, "duration_ms", logging.Duration(started), "error", logging.SafeSummary(err))
		return 0, nil, err
	}
	level := logging.Logger(ctx)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		level.Info("oneapi llm request", "endpoint", c.Endpoint+"/chat/completions", "model", c.Model, "status", resp.StatusCode, "duration_ms", logging.Duration(started))
	} else {
		level.Error("oneapi llm request failed", "endpoint", c.Endpoint+"/chat/completions", "model", c.Model, "status", resp.StatusCode, "duration_ms", logging.Duration(started), "error", logging.SafeBody(data))
	}
	return resp.StatusCode, data, nil
}

func structuredOutputUnsupported(data []byte) bool {
	message := strings.ToLower(string(data))
	return strings.Contains(message, "response_format") || strings.Contains(message, "json_schema") || strings.Contains(message, "structured output")
}

const defaultAnalyzePrompt = `Analyze the supplied source document into a concise structured knowledge analysis. Group chunks under distinct top-level concepts, entities, or topics. For every topic, page_type MUST be exactly one of "concept", "entity", or "topic". A schedule, plan, checklist, timeline, or task list is still a "topic" unless it is clearly an entity or concept; never output "plan" as page_type. Every claim_type MUST be exactly one of "fact", "definition", "argument", "procedure", or "caveat". Every relation MUST be exactly one of "related_to", "part_of", "depends_on", "contradicts", "supports", or "references". Every claim must cite one or more supplied source chunk IDs. Do not invent evidence IDs. Return JSON only.`
const defaultPlanPrompt = `Create a compilation plan from the analysis and candidates. Prefer UPDATE for a semantically matching existing page. Use only candidate page IDs for UPDATE, LINK, or MERGE. Every ADD, REVISE, SUPERSEDE, or MARK_DISPUTED claim must cite supplied source chunk IDs. Return JSON only.`
const defaultRewritePrompt = `Rewrite the follow-up into one concise standalone retrieval query using only the recent history supplied. Preserve the user's intent. Return JSON only.`
const defaultAnswerPrompt = `Answer only from the allowed context. Use Wiki context as the primary knowledge representation, but citation_ids must contain only exact source_evidence or source_fallback context IDs supplied in the prompt. If source evidence is insufficient, answer exactly "知识库证据不足，无法基于现有证据回答。" with an empty citation_ids array. Return JSON only.`
