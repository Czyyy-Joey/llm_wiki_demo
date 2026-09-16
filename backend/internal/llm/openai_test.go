package llm

import "testing"

func TestStripJSONFence(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", `{"a":1}`, `{"a":1}`},
		{"leading whitespace", "  \n{\"a\":1}\n", `{"a":1}`},
		{"fenced with json tag", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced no tag", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced with surrounding space", "  ```json\n{\"a\":1}\n```  ", `{"a":1}`},
		{"multiline body", "```json\n{\n  \"a\": 1\n}\n```", "{\n  \"a\": 1\n}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripJSONFence(tc.in); got != tc.want {
				t.Fatalf("stripJSONFence(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
