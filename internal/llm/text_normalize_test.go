package llm

import "testing"

func TestTrimLeadingBlankLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "none", in: "answer", want: "answer"},
		{name: "lf separator", in: "\n\nanswer", want: "answer"},
		{name: "crlf and padded blank line", in: "\r\n \t\r\nanswer", want: "answer"},
		{name: "first line indentation", in: "    code", want: "    code"},
		{name: "indentation after separator", in: "\n\tcode", want: "\tcode"},
		{name: "internal blank line", in: "answer\n\nnext", want: "answer\n\nnext"},
		{name: "whitespace only", in: "\r\n \t\n", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := TrimLeadingBlankLines(tt.in); got != tt.want {
				t.Fatalf("TrimLeadingBlankLines(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestLeadingBlankLineFilter_ChunkBoundariesAndReset(t *testing.T) {
	t.Parallel()
	var filter LeadingBlankLineFilter
	for _, chunk := range []string{"\r", "\n", " \t\n", "  "} {
		if got := filter.Push(chunk); got != "" {
			t.Fatalf("leading chunk %q emitted %q", chunk, got)
		}
	}
	if got := filter.Push("code\n\n"); got != "  code\n\n" {
		t.Fatalf("first authored chunk = %q", got)
	}
	if got := filter.Push("next"); got != "next" {
		t.Fatalf("post-start chunk = %q", got)
	}

	filter.Reset()
	if got := filter.Push("\n\nanswer"); got != "answer" {
		t.Fatalf("after reset = %q", got)
	}
}
