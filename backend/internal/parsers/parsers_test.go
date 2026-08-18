package parsers

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestMarkdownChunksPreserveHeadingPathAndByteRange(t *testing.T) {
	input := "# Alpha\n\nFirst paragraph.\nSecond line.\n\n## Beta\n\n中文 evidence.\n"
	chunks, err := Parse(Input{Name: "notes.md", Data: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunk count = %d, want 2", len(chunks))
	}
	if got := strings.Join(chunks[0].HeadingPath, "/"); got != "Alpha" {
		t.Fatalf("first heading path = %q", got)
	}
	if got := strings.Join(chunks[1].HeadingPath, "/"); got != "Alpha/Beta" {
		t.Fatalf("second heading path = %q", got)
	}
	for _, chunk := range chunks {
		if got := input[chunk.CharStart:chunk.CharEnd]; got != chunk.Text {
			t.Fatalf("range text = %q, chunk text = %q", got, chunk.Text)
		}
	}
}

func TestMarkdownFencedCodeBlocksRemainIntact(t *testing.T) {
	tests := []struct {
		name  string
		fence string
	}{
		{name: "backticks", fence: "```"},
		{name: "tildes", fence: "~~~"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := "# Parent\n\nBefore.\n\n" + test.fence + "go\n# comment\n\nvalue := 1\n" + test.fence + "\n\nAfter.\n\n## Child\n\nChild text.\n"
			chunks, err := Parse(Input{Name: "fenced.md", Data: []byte(input)})
			if err != nil {
				t.Fatal(err)
			}
			if len(chunks) != 4 {
				t.Fatalf("chunk count = %d, want 4: %#v", len(chunks), chunks)
			}
			code := chunks[1]
			expected := test.fence + "go\n# comment\n\nvalue := 1\n" + test.fence
			if code.Text != expected {
				t.Fatalf("fenced chunk = %q, want %q", code.Text, expected)
			}
			if got := strings.Join(code.HeadingPath, "/"); got != "Parent" {
				t.Fatalf("fenced heading path = %q", got)
			}
			if got := strings.Join(chunks[2].HeadingPath, "/"); got != "Parent" {
				t.Fatalf("post-fence heading path = %q", got)
			}
			if got := strings.Join(chunks[3].HeadingPath, "/"); got != "Parent/Child" {
				t.Fatalf("child heading path = %q", got)
			}
			if input[code.CharStart:code.CharEnd] != code.Text {
				t.Fatal("fenced chunk range does not locate its original text")
			}
		})
	}
}

func TestTextChunksByParagraphAndWordLimit(t *testing.T) {
	input := "first paragraph\n\nsecond paragraph"
	chunks, err := Parse(Input{Name: "notes.txt", MediaType: "application/octet-stream", Data: []byte(input)})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || chunks[0].Text != "first paragraph" || chunks[1].Text != "second paragraph" {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}

	words := make([]string, 401)
	for i := range words {
		words[i] = fmt.Sprintf("word%d", i)
	}
	longInput := strings.Join(words, " ")
	chunks, err = Parse(Input{Name: "long.txt", Data: []byte(longInput)})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("oversized chunk count = %d, want 2", len(chunks))
	}
	for _, chunk := range chunks {
		if longInput[chunk.CharStart:chunk.CharEnd] != chunk.Text {
			t.Fatalf("oversized chunk range does not locate its text")
		}
	}
}

func TestPDFChunksHavePageNumbers(t *testing.T) {
	data := testPDF(t, []string{"First page evidence", "Second page evidence"})
	chunks, err := Parse(Input{Name: "evidence.pdf", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunk count = %d, want 2: %#v", len(chunks), chunks)
	}
	for i, chunk := range chunks {
		if chunk.PageNumber == nil || *chunk.PageNumber != i+1 {
			t.Fatalf("chunk %d page = %v", i, chunk.PageNumber)
		}
		if !strings.Contains(chunk.Text, []string{"First page evidence", "Second page evidence"}[i]) {
			t.Fatalf("chunk %d text = %q", i, chunk.Text)
		}
	}
}

func TestParseRejectsUnsupportedAndUnextractableFiles(t *testing.T) {
	if _, err := Parse(Input{Name: "notes.docx", Data: []byte("content")}); err == nil {
		t.Fatal("unsupported file should fail")
	}
	if _, err := Parse(Input{Name: "empty.pdf", Data: testPDF(t, []string{""})}); err == nil || !strings.Contains(err.Error(), "no extractable text") {
		t.Fatalf("empty PDF error = %v", err)
	}
	if _, err := Parse(Input{Name: "blank.txt", MediaType: "text/plain; charset=utf-8", Data: []byte(" \n\t")}); err == nil || !strings.Contains(err.Error(), "no extractable text") {
		t.Fatalf("blank text error = %v", err)
	}
}

func testPDF(t *testing.T, pages []string) []byte {
	t.Helper()
	var objects []string
	pageRefs := make([]string, len(pages))
	for i := range pages {
		pageRefs[i] = fmt.Sprintf("%d 0 R", 3+i*2)
	}
	objects = append(objects,
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(pageRefs, " "), len(pages)),
	)
	for i, text := range pages {
		pageObject := 3 + i*2
		streamObject := pageObject + 1
		stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", escapePDFText(text))
		objects = append(objects,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>", 3+len(pages)*2, streamObject),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		)
	}
	widths := strings.TrimSpace(strings.Repeat("600 ", 95))
	objects = append(objects, fmt.Sprintf("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /FirstChar 32 /LastChar 126 /Widths [%s] >>", widths))

	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for i, object := range objects {
		offsets[i+1] = output.Len()
		fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&output, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}

func escapePDFText(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `(`, `\(`)
	return strings.ReplaceAll(value, `)`, `\)`)
}
