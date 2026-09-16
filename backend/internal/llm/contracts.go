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
type ChatTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type AnswerContext struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}
type GenerationClient interface {
	Rewrite(context.Context, string, []ChatTurn) (json.RawMessage, error)
	Answer(context.Context, string, []AnswerContext) (json.RawMessage, error)
}

type DeterministicEmbedding struct{}

func (DeterministicEmbedding) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	const dimensions = 64
	result := make([][]float32, len(inputs))
	for i, input := range inputs {
		vector := make([]float32, dimensions)
		for _, token := range strings.Fields(strings.ToLower(input)) {
			var hash uint32 = 2166136261
			for _, r := range token {
				hash ^= uint32(r)
				hash *= 16777619
			}
			vector[int(hash%dimensions)] += 1
		}
		result[i] = vector
	}
	return result, nil
}

type DeterministicFake struct{}

func (DeterministicFake) Name() string { return "deterministic-fake" }

func (DeterministicFake) Analyze(ctx context.Context, input AnalyzeInput) (json.RawMessage, error) {
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
	for _, marker := range []string{"plan", "schedule", "timeline", "checklist", "task", "计划", "时间表", "清单", "任务"} {
		if strings.Contains(lowerTitle, marker) {
			return domain.PageTypeTopic
		}
	}
	return domain.PageTypeConcept
}

func (DeterministicFake) Plan(ctx context.Context, input PlanInput) (json.RawMessage, error) {
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
		createReason, updateReason, noOpReason := "创建编译知识页面", "将来源证据融合到已有知识页面", "来源没有增加新 claim"
		if LanguageFromContext(ctx) == "en" {
			createReason, updateReason, noOpReason = "create a compiled knowledge page", "add source evidence to the existing knowledge page", "the source adds no new claims"
		}
		action := domain.PageAction{Action: domain.ActionCreate, Slug: topic.Slug, PageType: topic.PageType, Title: topic.Title, Summary: topic.Summary, Reason: createReason}
		if match != nil {
			matched[topic.Key] = *match
			action.Action = domain.ActionUpdate
			action.TargetPageID = match.PageID
			action.Reason = updateReason
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
			action.Reason = noOpReason
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

func (DeterministicFake) Rewrite(_ context.Context, question string, history []ChatTurn) (json.RawMessage, error) {
	query := strings.TrimSpace(question)
	if needsHistory(query) {
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Role == "user" && strings.TrimSpace(history[i].Content) != "" {
				query = strings.TrimSpace(history[i].Content) + " " + query
				break
			}
		}
	}
	return json.Marshal(domain.StandaloneQuery{Query: query})
}

func (DeterministicFake) Answer(ctx context.Context, _ string, contextItems []AnswerContext) (json.RawMessage, error) {
	insufficient := "知识库证据不足，无法基于现有证据回答。"
	if LanguageFromContext(ctx) == "en" {
		insufficient = "The knowledge base does not contain enough evidence to answer."
	}
	if len(contextItems) == 0 {
		return json.Marshal(domain.GeneratedAnswer{Answer: insufficient, CitationIDs: []string{}})
	}
	selected := -1
	for i := range contextItems {
		if strings.HasPrefix(contextItems[i].Kind, "source_") {
			selected = i
			break
		}
	}
	if selected < 0 {
		return json.Marshal(domain.GeneratedAnswer{Answer: insufficient, CitationIDs: []string{}})
	}
	text := strings.TrimSpace(contextItems[selected].Text)
	if len(text) > 360 {
		text = text[:360]
	}
	return json.Marshal(domain.GeneratedAnswer{Answer: text, CitationIDs: []string{contextItems[selected].ID}})
}

func needsHistory(question string) bool {
	lower := strings.ToLower(question)
	for _, marker := range []string{" it ", " they ", " them ", " that ", " this ", "其", "它", "这", "那", "上述", "前者", "后者"} {
		if strings.Contains(" "+lower+" ", marker) {
			return true
		}
	}
	return false
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
	typeOf := reflect.TypeFor[T]()
	schema := inlineSchema(huma.SchemaFromType(registry, typeOf), registry, map[string]bool{})
	applyDomainEnums(schema, dereference(typeOf))
	return schema
}

// applyDomainEnums keeps the provider-facing schema aligned with the domain's
// named string contracts. Reflection does not infer enum values from Go
// constants, so without this pass a structured-output provider can legally
// invent values such as "plan" for AnalyzedTopic.page_type.
func applyDomainEnums(schema *huma.Schema, typeOf reflect.Type) {
	if schema == nil || typeOf == nil {
		return
	}
	typeOf = dereference(typeOf)
	switch typeOf {
	case reflect.TypeOf(domain.PageType("")):
		schema.Enum = []any{string(domain.PageTypeConcept), string(domain.PageTypeEntity), string(domain.PageTypeTopic)}
		return
	case reflect.TypeOf(domain.ClaimType("")):
		schema.Enum = []any{string(domain.ClaimFact), string(domain.ClaimDefinition), string(domain.ClaimArgument), string(domain.ClaimProcedure), string(domain.ClaimCaveat)}
		return
	case reflect.TypeOf(domain.PlanAction("")):
		schema.Enum = []any{string(domain.ActionCreate), string(domain.ActionUpdate), string(domain.ActionMerge), string(domain.ActionLink), string(domain.ActionNoOp)}
		return
	}
	if typeOf.Kind() == reflect.Slice || typeOf.Kind() == reflect.Array {
		applyDomainEnums(schema.Items, dereference(typeOf.Elem()))
		return
	}
	if typeOf.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < typeOf.NumField(); i++ {
		field := typeOf.Field(i)
		if field.PkgPath != "" { // unexported
			continue
		}
		name := jsonFieldName(field)
		if name == "-" {
			continue
		}
		property := schema.Properties[name]
		if property == nil {
			continue
		}
		fieldType := dereference(field.Type)
		switch {
		case name == "action" && fieldType.Kind() == reflect.String && typeOf == reflect.TypeOf(domain.ClaimAction{}):
			property.Enum = []any{"ADD", "REVISE", "RETAIN", "MARK_DISPUTED", "SUPERSEDE"}
		case name == "relation" && fieldType.Kind() == reflect.String:
			property.Enum = []any{"related_to", "part_of", "depends_on", "contradicts", "supports", "references"}
		default:
			applyDomainEnums(property, fieldType)
		}
	}
}

func dereference(typeOf reflect.Type) reflect.Type {
	for typeOf != nil && (typeOf.Kind() == reflect.Pointer || typeOf.Kind() == reflect.Interface) {
		typeOf = typeOf.Elem()
	}
	return typeOf
}

func jsonFieldName(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "" {
		return field.Name
	}
	name := strings.Split(tag, ",")[0]
	if name == "" {
		return field.Name
	}
	return name
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
