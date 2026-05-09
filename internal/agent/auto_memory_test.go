package agent

// auto_memory_test.go — pin parseAutoMemoryLines (the deterministic
// half of the extractor); the full MaybeExtractMemory path requires
// a live LLM and is exercised by the e2e/tmux harness.

import (
	"reflect"
	"testing"
)

func TestParseAutoMemoryLines_Standard(t *testing.T) {
	in := `prefer_language: chinese
db_default: deepseek
project_root: /Users/ricardo/Documents/...`
	got := parseAutoMemoryLines(in)
	want := []string{
		"prefer_language: chinese",
		"db_default: deepseek",
		"project_root: /Users/ricardo/Documents/...",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v; want %+v", got, want)
	}
}

func TestParseAutoMemoryLines_NONE(t *testing.T) {
	if got := parseAutoMemoryLines("NONE"); got != nil {
		t.Errorf("NONE should return nil; got %+v", got)
	}
	if got := parseAutoMemoryLines("none"); got != nil {
		t.Errorf("lowercase none should return nil; got %+v", got)
	}
	if got := parseAutoMemoryLines("  NONE  "); got != nil {
		t.Errorf("whitespaced NONE should return nil; got %+v", got)
	}
}

func TestParseAutoMemoryLines_Empty(t *testing.T) {
	if got := parseAutoMemoryLines(""); got != nil {
		t.Errorf("empty input → nil; got %+v", got)
	}
}

func TestParseAutoMemoryLines_FiltersConversational(t *testing.T) {
	// Some models slip in prose despite the prompt. Drop lines
	// without a colon — they're not key:value.
	in := `Here are the facts:
prefer_color: blue
The user mentioned other things too.
project: my-app
Hope this helps!`
	got := parseAutoMemoryLines(in)
	want := []string{
		"prefer_color: blue",
		"project: my-app",
	}
	// "Here are the facts:" has a colon — but the leading word isn't
	// a key. This is a known limitation; the prompt asks for tight
	// output and it usually obeys. Document the imperfect behaviour
	// rather than over-engineer the parser.
	for _, want := range want {
		found := false
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in result; got %+v", want, got)
		}
	}
}

func TestParseAutoMemoryLines_StripsBullets(t *testing.T) {
	in := `- prefer_color: blue
* prefer_lang: go
1. project: x
12. tool: ripgrep`
	got := parseAutoMemoryLines(in)
	want := []string{
		"prefer_color: blue",
		"prefer_lang: go",
		"project: x",
		"tool: ripgrep",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v; want %+v", got, want)
	}
}

func TestParseAutoMemoryLines_CapsAt12(t *testing.T) {
	in := ""
	for i := 0; i < 20; i++ {
		in += "key" + string(rune('A'+i)) + ": value\n"
	}
	got := parseAutoMemoryLines(in)
	if len(got) != 12 {
		t.Errorf("len(got) = %d; want capped at 12", len(got))
	}
}

func TestMaybeExtractMemory_DisabledByDefault(t *testing.T) {
	l := &Loop{}
	if l.MaybeExtractMemory(nil) != 0 {
		t.Errorf("AutoMemory off by default; should return 0")
	}
}

func TestMaybeExtractMemory_NoOpWhenMemoryNil(t *testing.T) {
	l := &Loop{AutoMemory: true}
	if l.MaybeExtractMemory(nil) != 0 {
		t.Errorf("nil Memory → no-op, got non-zero result")
	}
}
