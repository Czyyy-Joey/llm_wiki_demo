package llm

import (
	"context"
	"strings"
)

type languageKey struct{}

func WithLanguage(ctx context.Context, language string) context.Context {
	if strings.EqualFold(strings.TrimSpace(language), "en") {
		return context.WithValue(ctx, languageKey{}, "en")
	}
	return context.WithValue(ctx, languageKey{}, "zh")
}

func LanguageFromContext(ctx context.Context) string {
	if value, ok := ctx.Value(languageKey{}).(string); ok && value == "en" {
		return "en"
	}
	return "zh"
}

func LanguageInstruction(ctx context.Context) string {
	if LanguageFromContext(ctx) == "en" {
		return "Respond in English. Keep JSON keys, enum values, IDs, and citation IDs unchanged."
	}
	return "Respond in Chinese. Keep JSON keys, enum values, IDs, and citation IDs unchanged."
}
