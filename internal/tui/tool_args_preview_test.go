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
	long := strings.Repeat("x", 200)
	got := previewStreamingArgs("Read", []byte(`{"path":"`+long+`"}`))
	if len(got) > 80 {
		t.Errorf("preview should stay under 80 chars; got %d (%q)", len(got), got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("truncated preview should end with ellipsis; got %q", got)
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

func TestTrimSnippet_TrimsTrailingWS(t *testing.T) {
	got := trimSnippet("hello    ", 100)
	if got != "hello" {
		t.Errorf("trailing whitespace should be trimmed; got %q", got)
	}
}
