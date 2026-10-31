package tui

import (
	"strings"
	"testing"
)

func TestPreviewStreamingArgs_FirstValueTypical(t *testing.T) {
	got := previewStreamingArgs("Read", []byte(`{"path":"/tmp/foo.go","limit":50}`))
	if !strings.Contains(got, "Read") || !strings.Contains(got, "/tmp/foo.go") {
		t.Errorf("expected tool · path; got %q", got)
	}
}

func TestPreviewStreamingArgs_PartialUnclosedValue(t *testing.T) {
	got := previewStreamingArgs("Bash", []byte(`{"command":"git stat`))
	// Value is partial — extractor returns "git stat" since no closing quote yet.
	if !strings.Contains(got, "git stat") {
		t.Errorf("partial value should still surface; got %q", got)
	}
}

func TestPreviewStreamingArgs_PreColonRawFallback(t *testing.T) {
	got := previewStreamingArgs("Edit", []byte(`{"file_pa`))
	// Before the colon — fallback shows the raw key fragment.
	if !strings.Contains(got, "file_pa") {
		t.Errorf("pre-colon partial should fall back to raw; got %q", got)
	}
}

func TestPreviewStreamingArgs_NumericValueFallsBack(t *testing.T) {
	got := previewStreamingArgs("Sleep", []byte(`{"duration_ms":5000`))
	// Numeric first value → not a string match → falls back to raw.
	if !strings.Contains(got, "duration_ms") {
		t.Errorf("numeric value should fall back to raw key fragment; got %q", got)
	}
}

func TestPreviewStreamingArgs_EmptyReturnsToolName(t *testing.T) {
	got := previewStreamingArgs("Glob", nil)
	if got != "Glob" {
		t.Errorf("empty buffer should return just the tool name; got %q", got)
	}
}

func TestPreviewStreamingArgs_LongValueTruncated(t *testing.T) {
	long := "/workspace/" + strings.Repeat("长目录/", 200) + "compaction.go"
	got := previewStreamingArgs("Read", []byte(`{"path":"`+long+`"}`))
	if len([]rune(got)) > toolArgsPreviewMaxRunes+len([]rune("Read · ")) {
		t.Errorf("preview exceeded defensive bound: %q", got)
	}
	if !strings.Contains(got, "…") || !strings.HasSuffix(got, "compaction.go") {
		t.Errorf("long preview must signal omission and retain filename; got %q", got)
	}
}

func TestPreviewStreamingArgs_PreservesOrdinaryPathWithoutFalseEllipsis(t *testing.T) {
	path := "/Users/ricardo/Documents/公司学习文件/我自己的agent的cli/metis/internal/agent/compaction.go"
	for _, raw := range []string{`{"path":"` + path + `"}`, `{"path":"` + path} {
		got := previewStreamingArgs("Read", []byte(raw))
		if got != "Read · "+path {
			t.Errorf("ordinary streamed path must reach the renderer intact: %q", got)
		}
	}
}

func TestPreviewStreamingArgs_DoesNotInventTruncationForShortValues(t *testing.T) {
	for _, tool := range []string{"Bash", ""} {
		for _, raw := range []string{`{"command":"git stat`, `{"file_pa`, `{"duration_ms":5000`} {
			if got := previewStreamingArgs(tool, []byte(raw)); strings.Contains(got, "…") {
				t.Errorf("untruncated partial args should not imply hidden content: %q", got)
			}
		}
	}
}

func TestPreviewStreamingArgs_EscapedQuoteTreatedAsContent(t *testing.T) {
	got := previewStreamingArgs("Bash", []byte(`{"command":"echo \"hi\""`))
	// The escaped quote should not terminate value parsing prematurely.
	if !strings.Contains(got, "echo") {
		t.Errorf("escaped quote should not break value extraction; got %q", got)
	}
}

func TestExtractFirstStringValue_OK(t *testing.T) {
	v, ok := extractFirstStringValue(`{"path":"/tmp"}`)
	if !ok || v != "/tmp" {
		t.Errorf("expected (/tmp, true); got (%q, %v)", v, ok)
	}
}

func TestExtractFirstStringValue_NoColon(t *testing.T) {
	if _, ok := extractFirstStringValue(`{"path"`); ok {
		t.Error("no colon yet → no string value")
	}
}

func TestPreviewStreamingArgs_KeepsStatusSingleLineWithoutChangingQuotedSpaces(t *testing.T) {
	got := previewStreamingArgs("Bash", []byte("{\"command\":\"echo 'a  b'\nnext\x1b[31m"))
	if got != "Bash · echo 'a  b' next" {
		t.Errorf("preview must remove terminal controls but preserve argument spacing: %q", got)
	}
}
