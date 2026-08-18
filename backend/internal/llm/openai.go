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

	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
)

type OpenAICompatible struct {
	Endpoint  string
	APIKey    string
	Model     string
	PromptDir string
	HTTP      *http.Client
}

type OpenAIEmbedding struct {
	Endpoint string
	APIKey   string
	Model    string
	HTTP     *http.Client
}

func NewOpenAIEmbedding(endpoint, apiKey, model string) *OpenAIEmbedding {
	return &OpenAIEmbedding{Endpoint: strings.TrimRight(endpoint, "/"), APIKey: apiKey, Model: model, HTTP: http.DefaultClient}
}

func (c *OpenAIEmbedding) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if c == nil || c.Endpoint == "" || c.APIKey == "" || c.Model == "" {
		return nil, fmt.Errorf("embedding provider is not configured")
	}
	body, err := json.Marshal(map[string]any{"model": c.Model, "input": inputs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var decoded struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}
	if decoded.Error != nil {
		return nil, fmt.Errorf("embedding provider: %s", decoded.Error.Message)
	}
	result := make([][]float32, len(inputs))
	for _, item := range decoded.Data {
		if item.Index < 0 || item.Index >= len(result) {
			return nil, fmt.Errorf("embedding response index %d out of range", item.Index)
		}
		result[item.Index] = item.Embedding
	}
	for i := range result {
		if len(result[i]) == 0 {
			return nil, fmt.Errorf("embedding response missing vector %d", i)
		}
	}
	return result, nil
}

func NewOpenAICompatible(endpoint, apiKey, model, promptDir string) *OpenAICompatible {
	return &OpenAICompatible{Endpoint: strings.TrimRight(endpoint, "/"), APIKey: apiKey, Model: model, PromptDir: promptDir, HTTP: http.DefaultClient}
}

func (c *OpenAICompatible) Name() string { return c.Model }

func (c *OpenAICompatible) Analyze(ctx context.Context, input AnalyzeInput) (json.RawMessage, error) {
	chunks, _ := json.Marshal(input.Chunks)
	document, _ := json.Marshal(input.Document)
	schema, _ := json.Marshal(SchemaFor[domain.SourceAnalysis]())
	prompt := c.prompt("analyze.txt", defaultAnalyzePrompt)
	return c.complete(ctx, prompt, fmt.Sprintf("Document:\n%s\n\nSource chunks:\n%s", document, chunks), "source_analysis", schema)
}

func (c *OpenAICompatible) Plan(ctx context.Context, input PlanInput) (json.RawMessage, error) {
	schema, _ := json.Marshal(SchemaFor[domain.CompilationPlan]())
	prompt := c.prompt("plan.txt", defaultPlanPrompt)
	return c.complete(ctx, prompt, fmt.Sprintf("Document ID: %s\n\nAnalysis:\n%s\n\nCandidates:\n%s", input.DocumentID, input.Analysis, input.Candidates), "compilation_plan", schema)
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
	Type       string         `json:"type"`
	JSONSchema responseSchema `json:"json_schema"`
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
		return nil, fmt.Errorf("compiler LLM provider is not configured")
	}
	body, err := json.Marshal(chatRequest{Model: c.Model, Messages: []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}}, ResponseFormat: responseFormat{Type: "json_schema", JSONSchema: responseSchema{Name: name, Strict: true, Schema: schema}}})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("compiler LLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var result chatResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode compiler LLM response: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("compiler LLM error: %s", result.Error.Message)
	}
	if len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "" {
		return nil, fmt.Errorf("compiler LLM returned no structured content")
	}
	return json.RawMessage(result.Choices[0].Message.Content), nil
}

const defaultAnalyzePrompt = `Analyze the supplied source document into a concise structured knowledge analysis. Group chunks under the correct top-level concept, entity, or topic. Every claim must cite one or more supplied source chunk IDs. Do not invent evidence IDs. Return JSON only.`
const defaultPlanPrompt = `Create a compilation plan from the analysis and candidates. Prefer UPDATE for a semantically matching existing page. Use only candidate page IDs for UPDATE, LINK, or MERGE. Every ADD, REVISE, SUPERSEDE, or MARK_DISPUTED claim must cite supplied source chunk IDs. Return JSON only.`
