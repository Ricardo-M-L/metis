package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssembleSystemPrompt_NoFileAddsEnvBlock(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	got := AssembleSystemPrompt("you are metis")
	// Base prompt always present.
	if !strings.Contains(got, "you are metis") {
		t.Error("base prompt missing")
	}
	// Env block always present (so the LLM doesn't hallucinate paths).
	if !strings.Contains(got, "<env>") || !strings.Contains(got, "</env>") {
		t.Error("env block missing — LLM will guess /home/user style paths")
	}
	if !strings.Contains(got, "Working directory:") || !strings.Contains(got, "Home directory:") {
		t.Errorf("env block content missing; got %q", got)
	}
	if !strings.Contains(got, "Local date and time:") || !strings.Contains(got, "Local timezone:") || !strings.Contains(got, "UTC") {
		t.Errorf("env block must expose the detected local time and UTC offset; got %q", got)
	}
}

func TestFormatUTCOffset(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    string
	}{
		{seconds: 8 * 60 * 60, want: "UTC+08:00"},
		{seconds: -(3*60*60 + 30*60), want: "UTC-03:30"},
		{seconds: 0, want: "UTC+00:00"},
	} {
		if got := formatUTCOffset(tc.seconds); got != tc.want {
			t.Errorf("formatUTCOffset(%d) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

func TestAssembleSystemPrompt_AppendsFileContent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	addendum := "always reply in Chinese.\nbe concise."
	if err := os.WriteFile(filepath.Join(dir, SystemPromptFileName),
		[]byte(addendum), 0o644); err != nil {
		t.Fatal(err)
	}
	got := AssembleSystemPrompt("you are metis")
	if !strings.Contains(got, "you are metis") {
		t.Error("base prompt missing")
	}
	if !strings.Contains(got, "always reply in Chinese") {
		t.Error("addendum missing")
	}
	// Order: base → env block → addendum. Addendum must come AFTER </env>
	// so user overrides ("speak Chinese") win over env-block phrasing.
	envEnd := strings.Index(got, "</env>")
	addIdx := strings.Index(got, "always reply in Chinese")
	if envEnd < 0 || addIdx < 0 || envEnd >= addIdx {
		t.Errorf("addendum should appear after </env> tag; got %q", got)
	}
}

func TestAssembleSystemPrompt_TrimsAddendum(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, SystemPromptFileName),
		[]byte("\n\n  hello\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := AssembleSystemPrompt("base")
	if strings.HasSuffix(got, "\n") {
		t.Errorf("trailing newline should be stripped; got %q", got)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("addendum content lost; got %q", got)
	}
}

func TestAssembleSystemPrompt_EmptyFileStillAddsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, SystemPromptFileName),
		[]byte("   \n  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := AssembleSystemPrompt("base")
	// Whitespace-only addendum is ignored, but env block is still there
	// (the bug we want to never regress on).
	if !strings.Contains(got, "<env>") {
		t.Errorf("env block should always be added even with empty addendum; got %q", got)
	}
	if strings.Contains(got, "always") {
		// (no real addendum text expected)
		t.Errorf("phantom addendum content; got %q", got)
	}
}
