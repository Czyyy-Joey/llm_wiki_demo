package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/joeychen/llm-wiki-demo/backend/internal/domain"
)

type AnalyzeInput struct {
	Document domain.SourceDocument
	Chunks   []domain.SourceChunk
}
type PlanInput struct {
	DocumentID string
	Analysis   json.RawMessage
	Candidates json.RawMessage
}
type LLMClient interface {
	Analyze(context.Context, AnalyzeInput) (json.RawMessage, error)
	Plan(context.Context, PlanInput) (json.RawMessage, error)
}
type EmbeddingClient interface {
	Embed(context.Context, []string) ([][]float32, error)
}

type DeterministicFake struct{}

func (DeterministicFake) Name() string { return "deterministic-fake" }

func (DeterministicFake) Analyze(_ context.Context, input AnalyzeInput) (json.RawMessage, error) {
	type topic struct {
		Key      string           `json:"key"`
		Title    string           `json:"title"`
		Slug     string           `json:"slug"`
		PageType domain.PageType  `json:"page_type"`
		Summary  string           `json:"summary"`
		Claims   []map[string]any `json:"claims"`
	}
	topics := make([]topic, 0)
	byKey := make(map[string]int)
	for _, chunk := range input.Chunks {
		title := input.Document.OriginalName
		if len(chunk.HeadingPath) > 0 && strings.TrimSpace(chunk.HeadingPath[0]) != "" {
			title = chunk.HeadingPath[0]
			if len(chunk.HeadingPath) > 1 {
				title = chunk.HeadingPath[len(chunk.HeadingPath)-1]
			}
		}
		key := slugify(title)
		if key == "" {
			key = "source"
		}
		index, ok := byKey[key]
		if !ok {
			byKey[key] = len(topics)
			topics = append(topics, topic{Key: key, Title: title, Slug: key, PageType: fakePageType(title), Summary: chunk.Text, Claims: make([]map[string]any, 0)})
			index = len(topics) - 1
		}
		topics[index].Claims = append(topics[index].Claims, map[string]any{"text": fakeClaimText(chunk.Text), "claim_type": string(domain.ClaimFact), "evidence_chunk_ids": []string{chunk.ID}})
	}
	if len(topics) == 0 {
		title := input.Document.OriginalName
		key := slugify(title)
		if key == "" {
			key = "source"
		}
		topics = append(topics, topic{Key: key, Title: title, Slug: key, PageType: fakePageType(title), Summary: "No parsed evidence", Claims: make([]map[string]any, 0)})
	}
	relations := make([]map[string]string, 0)
	for i := 1; i < len(topics); i++ {
		relations = append(relations, map[string]string{"source_topic_key": topics[i-1].Key, "target_topic_key": topics[i].Key, "relation": "related_to"})
	}
	return json.Marshal(map[string]any{"document_id": input.Document.ID, "summary": firstSummary(input.Chunks), "topics": topics, "relations": relations})
}

func fakeClaimText(text string) string {
	value := strings.Join(strings.Fields(text), " ")
	for _, delimiter := range []string{". ", "! ", "? "} {
		if index := strings.Index(value, delimiter); index >= 0 {
			value = value[:index+1]
			break
		}
	}
	if len(value) > 240 {
		value = strings.TrimSpace(value[:240])
	}
	return value
}

func fakePageType(title string) domain.PageType {
	lowerTitle := strings.ToLower(title)
	if strings.Contains(lowerTitle, "entity") {
		return domain.PageTypeEntity
	}
	if strings.Contains(lowerTitle, "topic") {
		return domain.PageTypeTopic
	}
	return domain.PageTypeConcept
}

func (DeterministicFake) Plan(_ context.Context, input PlanInput) (json.RawMessage, error) {
	var analysis struct {
		DocumentID string `json:"document_id"`
		Topics     []struct {
			Key, Slug, Title, Summary string
			PageType                  domain.PageType `json:"page_type"`
			Claims                    []struct {
				Text      string           `json:"text"`
				ClaimType domain.ClaimType `json:"claim_type"`
				Evidence  []string         `json:"evidence_chunk_ids"`
			} `json:"claims"`
		} `json:"topics"`
		Relations []struct {
			Source   string `json:"source_topic_key"`
			Target   string `json:"target_topic_key"`
			Relation string `json:"relation"`
		} `json:"relations"`
	}
	if err := json.Unmarshal(input.Analysis, &analysis); err != nil {
		return nil, err
	}
	var candidates []domain.CompilationCandidate
	if err := json.Unmarshal(input.Candidates, &candidates); err != nil {
		return nil, err
	}
	actions := make([]domain.PageAction, 0, len(analysis.Topics))
	matched := make(map[string]domain.CompilationCandidate)
	for _, topic := range analysis.Topics {
		var match *domain.CompilationCandidate
		var alternate *domain.CompilationCandidate
		for i := range candidates {
			if candidates[i].TopicKey == topic.Key && candidates[i].Score >= 0.60 {
				if match == nil {
					match = &candidates[i]
				} else if alternate == nil && candidates[i].PageID != match.PageID && candidates[i].Score >= 0.85 {
					alternate = &candidates[i]
				}
			}
		}
		action := domain.PageAction{Action: domain.ActionCreate, Slug: topic.Slug, PageType: topic.PageType, Title: topic.Title, Summary: topic.Summary, Reason: "create a compiled knowledge page"}
		if match != nil {
			matched[topic.Key] = *match
			action.Action = domain.ActionUpdate
			action.TargetPageID = match.PageID
			action.Reason = "add source evidence to the existing knowledge page"
		}
		for _, claim := range topic.Claims {
			duplicate := false
			if match != nil {
				for _, existing := range match.ClaimTexts {
					if existing == claim.Text {
						duplicate = true
						break
					}
				}
			}
			if !duplicate {
				action.ClaimActions = append(action.ClaimActions, domain.ClaimAction{Action: "ADD", Text: claim.Text, ClaimType: claim.ClaimType, EvidenceChunkIDs: claim.Evidence})
			}
		}
		if match != nil && len(action.ClaimActions) == 0 {
			action.Action = domain.ActionNoOp
			action.Reason = "the source adds no new claims"
		}
		actions = append(actions, action)
		if match != nil && alternate != nil {
			actions = append(actions, domain.PageAction{
				Action:       domain.ActionMerge,
				SourcePageID: alternate.PageID,
				TargetPageID: match.PageID,
				Reason:       "merge a high-confidence duplicate page into the best existing match",
			})
		}
	}
	for _, relation := range analysis.Relations {
		source, sourceOK := matched[relation.Source]
		target, targetOK := matched[relation.Target]
		if sourceOK && targetOK && source.PageID != target.PageID {
			actions = append(actions, domain.PageAction{Action: domain.ActionLink, SourcePageID: source.PageID, TargetPageID: target.PageID, Relation: relation.Relation, Reason: "link related existing knowledge pages"})
		}
	}
	return json.Marshal(domain.CompilationPlan{DocumentID: input.DocumentID, PageActions: actions, SchemaVersion: "phase2-v1"})
}

func (DeterministicFake) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	result := make([][]float32, len(inputs))
	for i, input := range inputs {
		result[i] = []float32{float32(len(input)), float32(len(strings.Fields(input)))}
	}
	return result, nil
}

func firstSummary(chunks []domain.SourceChunk) string {
	if len(chunks) == 0 {
		return ""
	}
	value := chunks[0].Text
	if len(value) > 240 {
		value = value[:240]
	}
	return value
}
func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func DecodeStrict[T any](data []byte) (T, error) {
	var value T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode structured output: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return value, err
	}
	return value, nil
}

func SchemaFor[T any]() *huma.Schema {
	registry := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	return inlineSchema(huma.SchemaFromType(registry, reflect.TypeFor[T]()), registry, map[string]bool{})
}

func inlineSchema(schema *huma.Schema, registry huma.Registry, resolving map[string]bool) *huma.Schema {
	if schema == nil {
		return nil
	}
	if schema.Ref != "" {
		if resolving[schema.Ref] {
			return schema
		}
		resolved := registry.SchemaFromRef(schema.Ref)
		if resolved == nil {
			return schema
		}
		resolving[schema.Ref] = true
		inlined := inlineSchema(resolved, registry, resolving)
		delete(resolving, schema.Ref)
		return inlined
	}
	copy := *schema
	copy.Ref = ""
	copy.Items = inlineSchema(schema.Items, registry, resolving)
	if schema.Properties != nil {
		copy.Properties = make(map[string]*huma.Schema, len(schema.Properties))
		for name, property := range schema.Properties {
			copy.Properties[name] = inlineSchema(property, registry, resolving)
		}
	}
	copy.OneOf = inlineSchemas(schema.OneOf, registry, resolving)
	copy.AnyOf = inlineSchemas(schema.AnyOf, registry, resolving)
	copy.AllOf = inlineSchemas(schema.AllOf, registry, resolving)
	copy.Not = inlineSchema(schema.Not, registry, resolving)
	return &copy
}

func inlineSchemas(schemas []*huma.Schema, registry huma.Registry, resolving map[string]bool) []*huma.Schema {
	if schemas == nil {
		return nil
	}
	result := make([]*huma.Schema, len(schemas))
	for i, schema := range schemas {
		result[i] = inlineSchema(schema, registry, resolving)
	}
	return result
}

func ensureEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing structured output: %w", err)
	}
	return errors.New("structured output contains trailing JSON")
}
