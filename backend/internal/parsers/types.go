package parsers

import (
	"fmt"
	"strings"
)

const ParserVersion = "phase1-v2"

type Input struct {
	Name      string
	MediaType string
	Data      []byte
}

type Chunk struct {
	Index       int
	Text        string
	PageNumber  *int
	HeadingPath []string
	CharStart   int
	CharEnd     int
	preserve    bool
}

func Parse(input Input) (chunks []Chunk, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			chunks = nil
			err = fmt.Errorf("parse %q: %v", input.Name, recovered)
		}
	}()

	switch NormalizeMediaType(input.Name, input.MediaType) {
	case "text/markdown", "text/x-markdown":
		chunks, err = parseMarkdown(string(input.Data))
	case "text/plain":
		chunks, err = parseText(string(input.Data))
	case "application/pdf":
		chunks, err = parsePDF(input.Data)
	default:
		return nil, fmt.Errorf("unsupported media type for %q", input.Name)
	}
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("%q contains no extractable text", input.Name)
	}
	return chunks, nil
}

func NormalizeMediaType(name, declared string) string {
	declared = strings.TrimSpace(strings.ToLower(strings.SplitN(declared, ";", 2)[0]))
	if declared != "" && declared != "application/octet-stream" {
		return declared
	}
	switch extension(name) {
	case ".md", ".markdown":
		return "text/markdown"
	case ".txt":
		return "text/plain"
	case ".pdf":
		return "application/pdf"
	default:
		return declared
	}
}

func extension(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			return lower(name[i:])
		}
	}
	return ""
}

func lower(value string) string {
	result := make([]byte, len(value))
	for i := range value {
		if value[i] >= 'A' && value[i] <= 'Z' {
			result[i] = value[i] + ('a' - 'A')
		} else {
			result[i] = value[i]
		}
	}
	return string(result)
}
