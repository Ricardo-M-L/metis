package runtime

import (
	"strings"
	"testing"
)

func TestSimpleBasePrompt_ContainsModelAndDate(t *testing.T) {
	out := SimpleBasePrompt("MiniMax-M2.7")
	if !strings.Contains(out, "MiniMax-M2.7") {
		t.Errorf("simple prompt should mention model; got:\n%s", out)
	}
	if !strings.Contains(out, "CWD:") {
		t.Errorf("simple prompt should mention CWD; got:\n%s", out)
	}
	if !strings.Contains(out, "Date:") {
		t.Errorf("simple prompt should mention Date; got:\n%s", out)
	}
	// Sanity: dramatically shorter than the full base prompt.
	full := RenderBasePrompt(BasePromptVars{Model: "MiniMax-M2.7"})
	if len(out) >= len(full)/4 {
		t.Errorf("simple prompt (%d chars) should be << full (%d chars)", len(out), len(full))
	}
}

func TestSimpleBasePrompt_NoModelFallback(t *testing.T) {
	out := SimpleBasePrompt("")
	if !strings.Contains(out, "an LLM") {
		t.Errorf("empty model should fall back to 'an LLM'; got:\n%s", out)
	}
}

func TestIsSimpleMode_EnvTruthy(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"no":    false,
		"1":     true,
		"true":  true,
		"yes":   true,
		"on":    true,
		"TRUE":  true, // case-insensitive
		"  1  ": true, // trim
	}
	for v, want := range cases {
		t.Run("METIS_SIMPLE="+v, func(t *testing.T) {
			t.Setenv("METIS_SIMPLE", v)
			if got := IsSimpleMode(); got != want {
				t.Errorf("IsSimpleMode() with METIS_SIMPLE=%q = %v, want %v", v, got, want)
			}
		})
	}
}
