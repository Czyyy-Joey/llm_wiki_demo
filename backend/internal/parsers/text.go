package parsers

import (
	"regexp"
	"strings"
)

var headingPattern = regexp.MustCompile(`^(#{1,6})[ \t]+(.+?)\s*$`)
var wordPattern = regexp.MustCompile(`\S+`)

func parseMarkdown(text string) ([]Chunk, error) {
	return parseBlocks(text, true, nil)
}

func parseText(text string) ([]Chunk, error) {
	return parseBlocks(text, false, nil)
}

func parseBlocks(text string, headings bool, pageNumber *int) ([]Chunk, error) {
	var chunks []Chunk
	var headingPath []string
	var fence byte
	var fenceLength int
	blockPreserve := false
	blockStart := -1
	blockEnd := -1
	flush := func(_ int) {
		if blockStart < 0 {
			return
		}
		start, contentEnd := trimRange(text, blockStart, blockEnd)
		if start == contentEnd {
			blockStart = -1
			blockEnd = -1
			return
		}
		chunks = append(chunks, Chunk{Index: len(chunks), Text: text[start:contentEnd], PageNumber: pageNumber, HeadingPath: append([]string(nil), headingPath...), CharStart: start, CharEnd: contentEnd, preserve: blockPreserve})
		blockStart = -1
		blockEnd = -1
		blockPreserve = false
	}
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		lineText := strings.TrimSuffix(line, "\n")
		if headings && fence != 0 {
			blockEnd = offset + len(lineText)
			if isClosingFence(lineText, fence, fenceLength) {
				fence = 0
				fenceLength = 0
				flush(offset + len(line))
			}
			offset += len(line)
			continue
		}
		if headings {
			if marker, length, ok := openingFence(lineText); ok {
				flush(offset)
				fence = marker
				fenceLength = length
				blockStart = offset
				blockEnd = offset + len(lineText)
				blockPreserve = true
				offset += len(line)
				continue
			}
			if match := headingPattern.FindStringSubmatch(lineText); match != nil {
				flush(offset)
				level := len(match[1])
				if len(headingPath) >= level {
					headingPath = headingPath[:level-1]
				}
				headingPath = append(headingPath, strings.TrimSpace(match[2]))
				offset += len(line)
				continue
			}
		}
		if strings.TrimSpace(lineText) == "" {
			flush(offset)
			offset += len(line)
			continue
		}
		if blockStart < 0 {
			blockStart = offset
		}
		blockEnd = offset + len(lineText)
		offset += len(line)
	}
	flush(len(text))
	return splitOversized(chunks), nil
}

func openingFence(line string) (byte, int, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return 0, 0, false
	}
	marker := trimmed[0]
	length := 0
	for length < len(trimmed) && trimmed[length] == marker {
		length++
	}
	return marker, length, length >= 3
}

func isClosingFence(line string, marker byte, minimumLength int) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < minimumLength {
		return false
	}
	for i := range trimmed {
		if trimmed[i] != marker {
			return false
		}
	}
	return true
}

func trimRange(text string, start, end int) (int, int) {
	for start < end && isSpace(text[start]) {
		start++
	}
	for end > start && isSpace(text[end-1]) {
		end--
	}
	return start, end
}

func isSpace(value byte) bool {
	switch value {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

func splitOversized(chunks []Chunk) []Chunk {
	const maxWords = 400
	var result []Chunk
	for _, chunk := range chunks {
		words := wordPattern.FindAllStringIndex(chunk.Text, -1)
		if chunk.preserve || len(words) <= maxWords {
			chunk.Index = len(result)
			result = append(result, chunk)
			continue
		}
		for start := 0; start < len(words); start += maxWords {
			end := start + maxWords
			if end > len(words) {
				end = len(words)
			}
			partStart := chunk.CharStart + words[start][0]
			partEnd := chunk.CharStart + words[end-1][1]
			result = append(result, Chunk{Index: len(result), Text: chunk.Text[words[start][0]:words[end-1][1]], PageNumber: chunk.PageNumber, HeadingPath: append([]string(nil), chunk.HeadingPath...), CharStart: partStart, CharEnd: partEnd})
		}
	}
	return result
}
