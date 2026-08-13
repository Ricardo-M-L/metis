package llm

// TrimLeadingBlankLines removes only complete blank lines before the first
// authored line. Horizontal indentation on that first line is retained, as
// are every newline and space after its first non-whitespace byte.
//
// Some OpenAI-compatible reasoning endpoints emit blank content lines when
// switching from reasoning_content to content. Those bytes are a transport
// separator rather than assistant prose. strings.TrimSpace is deliberately
// not used here: it would corrupt an answer whose first line is indented code.
func TrimLeadingBlankLines(text string) string {
	start := 0
	for start < len(text) {
		lineEnd, breakLen := nextLineBreak(text, start)
		if lineEnd < 0 {
			if horizontalWhitespaceOnly(text[start:]) {
				return ""
			}
			return text[start:]
		}
		if !horizontalWhitespaceOnly(text[start:lineEnd]) {
			return text[start:]
		}
		start = lineEnd + breakLen
	}
	return ""
}

func nextLineBreak(text string, start int) (at, size int) {
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '\n':
			return i, 1
		case '\r':
			if i+1 < len(text) && text[i+1] == '\n' {
				return i, 2
			}
			return i, 1
		}
	}
	return -1, 0
}

func horizontalWhitespaceOnly(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] != ' ' && text[i] != '\t' {
			return false
		}
	}
	return true
}

// LeadingBlankLineFilter applies TrimLeadingBlankLines across arbitrary
// streaming chunk boundaries. It buffers only the undecidable leading ASCII
// whitespace. Once the first authored byte arrives, Push becomes a zero-copy
// pass-through for the rest of that content block.
type LeadingBlankLineFilter struct {
	pending string
	started bool
}

// Push accepts one provider text delta and returns the part safe to expose to
// live consumers. An empty return means the delta was either empty or still
// consists solely of an undecidable/blank leading prefix.
func (f *LeadingBlankLineFilter) Push(delta string) string {
	if delta == "" {
		return ""
	}
	if f.started {
		return delta
	}
	f.pending += delta
	if asciiWhitespaceOnly(f.pending) {
		return ""
	}
	text := TrimLeadingBlankLines(f.pending)
	f.pending = ""
	f.started = true
	return text
}

// Reset starts normalization for a new assistant text content block. Any
// pending whitespace-only prefix is intentionally discarded.
func (f *LeadingBlankLineFilter) Reset() {
	f.pending = ""
	f.started = false
}

func asciiWhitespaceOnly(text string) bool {
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case ' ', '\t', '\r', '\n':
		default:
			return false
		}
	}
	return true
}
