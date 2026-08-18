package parsers

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"rsc.io/pdf"
)

func parsePDF(data []byte) ([]Chunk, error) {
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open PDF: %w", err)
	}
	var chunks []Chunk
	for page := 1; page <= reader.NumPage(); page++ {
		text := pageText(reader.Page(page).Content().Text)
		if text == "" {
			continue
		}
		pageNumber := page
		pageChunks, _ := parseBlocks(text, false, &pageNumber)
		for _, chunk := range pageChunks {
			chunk.Index = len(chunks)
			chunks = append(chunks, chunk)
		}
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("PDF contains no extractable text")
	}
	return chunks, nil
}

func pageText(items []pdf.Text) string {
	sort.Sort(pdf.TextVertical(items))
	var result strings.Builder
	var previous *pdf.Text
	for i := range items {
		item := &items[i]
		if strings.TrimSpace(item.S) == "" {
			continue
		}
		if previous != nil {
			lineTolerance := item.FontSize * 0.35
			if lineTolerance < 1 {
				lineTolerance = 1
			}
			if abs(previous.Y-item.Y) > lineTolerance {
				result.WriteByte('\n')
			} else if item.X > previous.X+previous.W+0.01 {
				result.WriteByte(' ')
			}
		}
		result.WriteString(item.S)
		previous = item
	}
	return strings.TrimSpace(result.String())
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
