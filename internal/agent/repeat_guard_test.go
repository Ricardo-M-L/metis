package agent

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func gb(name string, in map[string]any) llm.ContentBlock {
	return llm.ContentBlock{Type: "tool_use", ToolName: name, ToolInput: in}
}

// feed runs a sequence of single-tool steps through the guard and returns
// every non-empty reminder in order.
func feed(g *RepeatGuard, blocks []llm.ContentBlock) []string {
	var out []string
	for _, b := range blocks {
		if r := g.RecordStep([]llm.ContentBlock{b}); r != "" {
			out = append(out, r)
		}
	}
	return out
}

func TestRepeatGuard_FirstThresholdGenericNudge(t *testing.T) {
	g := NewRepeatGuard(RepeatGuardConfig{})
	r := feed(g, []llm.ContentBlock{
		gb("Grep", map[string]any{"pattern": "x"}),
		gb("Grep", map[string]any{"pattern": "x"}),
		gb("Grep", map[string]any{"pattern": "x"}),
	})
	if len(r) != 1 {
		t.Fatalf("expected exactly one reminder at count 3, got %d", len(r))
	}
	if r[0] != repeatGuardFirstNudge {
		t.Fatalf("first threshold should be the generic nudge, got %q", r[0])
	}
}

func TestRepeatGuard_LaterThresholdDetailed(t *testing.T) {
	g := NewRepeatGuard(RepeatGuardConfig{})
	var last string
	for i := 0; i < 5; i++ {
		last = ""
		if r := g.RecordStep([]llm.ContentBlock{gb("Edit", map[string]any{"path": "a.go"})}); r != "" {
			last = r
		}
	}
	if last == "" {
		t.Fatal("expected a detailed reminder at count 5")
	}
	for _, want := range []string{"- tool: Edit", "- consecutive_calls: 5", "- arguments: {\"path\":\"a.go\"}"} {
		if !strings.Contains(last, want) {
			t.Fatalf("detailed reminder missing %q:\n%s", want, last)
		}
	}
}

func TestRepeatGuard_DifferentArgsResetsChain(t *testing.T) {
	g := NewRepeatGuard(RepeatGuardConfig{})
	got := feed(g, []llm.ContentBlock{
		gb("Grep", map[string]any{"pattern": "a"}),
		gb("Grep", map[string]any{"pattern": "a"}),
		gb("Grep", map[string]any{"pattern": "b"}), // different → reset to 1
		gb("Grep", map[string]any{"pattern": "b"}),
		gb("Grep", map[string]any{"pattern": "b"}), // count 3 → fires
	})
	if len(got) != 1 {
		t.Fatalf("expected one reminder (only the b-run reaches 3), got %d", len(got))
	}
}

func TestRepeatGuard_CanonicalizationIgnoresKeyOrder(t *testing.T) {
	g := NewRepeatGuard(RepeatGuardConfig{})
	r := feed(g, []llm.ContentBlock{
		gb("Read", map[string]any{"path": "f", "offset": 1}),
		gb("Read", map[string]any{"offset": 1, "path": "f"}),
		gb("Read", map[string]any{"path": "f", "offset": 1}),
	})
	if len(r) != 1 {
		t.Fatalf("key-order-only difference must count as identical, got %d reminders", len(r))
	}
}

func TestRepeatGuard_ExcludedToolTransparent(t *testing.T) {
	g := NewRepeatGuard(RepeatGuardConfig{}) // default exclude: todo_write
	r := feed(g, []llm.ContentBlock{
		gb("Grep", map[string]any{"pattern": "x"}),
		gb("TodoWrite", map[string]any{"todos": []any{}}), // transparent
		gb("Grep", map[string]any{"pattern": "x"}),
		gb("Grep", map[string]any{"pattern": "x"}), // still consecutive count 3
	})
	if len(r) != 1 {
		t.Fatalf("excluded tool must not launder the chain, got %d reminders", len(r))
	}
}

func TestRepeatGuard_IncludeFilters(t *testing.T) {
	g := NewRepeatGuard(RepeatGuardConfig{Include: []string{"Grep", "Read"}})
	r := feed(g, []llm.ContentBlock{
		gb("Edit", map[string]any{"path": "a"}),
		gb("Edit", map[string]any{"path": "a"}),
		gb("Edit", map[string]any{"path": "a"}),
	})
	if len(r) != 0 {
		t.Fatalf("Edit not in include list must be ignored, got %d", len(r))
	}
}

func TestRepeatGuard_ResetClearsChain(t *testing.T) {
	g := NewRepeatGuard(RepeatGuardConfig{})
	feed(g, []llm.ContentBlock{gb("Grep", map[string]any{"pattern": "x"}), gb("Grep", map[string]any{"pattern": "x"})})
	g.Reset() // fresh user turn
	r := feed(g, []llm.ContentBlock{gb("Grep", map[string]any{"pattern": "x"}), gb("Grep", map[string]any{"pattern": "x"}), gb("Grep", map[string]any{"pattern": "x"})})
	if len(r) != 1 {
		t.Fatalf("reset must restart the chain, got %d reminders", len(r))
	}
}

func TestRepeatGuard_ArgumentPreviewCap(t *testing.T) {
	g := NewRepeatGuard(RepeatGuardConfig{ArgumentsPreviewChars: 20})
	long := map[string]any{"text": strings.Repeat("z", 200)}
	var last string
	for i := 0; i < 8; i++ {
		if r := g.RecordStep([]llm.ContentBlock{gb("Write", long)}); r != "" {
			last = r
		}
	}
	if last == "" {
		t.Fatal("expected a reminder at count 8")
	}
	if !strings.Contains(last, "(+") || !strings.Contains(last, "more chars)") {
		t.Fatalf("capped preview must carry the omitted-count marker:\n%s", last)
	}
	if strings.Contains(last, strings.Repeat("z", 200)) {
		t.Fatal("full argument body must not appear in the reminder")
	}
}

func TestRepeatGuard_InvalidThresholdsNormalized(t *testing.T) {
	g := NewRepeatGuard(RepeatGuardConfig{Thresholds: []int{1, 3, 3, 0, -2}})
	if len(g.thresholds) != 1 || g.thresholds[0] != 3 {
		t.Fatalf("invalid/duplicate thresholds must be dropped, got %v", g.thresholds)
	}
}

func TestRepeatGuard_ThresholdOrderAscending(t *testing.T) {
	g := NewRepeatGuard(RepeatGuardConfig{Thresholds: []int{8, 3, 5}})
	want := []int{3, 5, 8}
	for i := range want {
		if g.thresholds[i] != want[i] {
			t.Fatalf("thresholds must normalize to ascending order, got %v", g.thresholds)
		}
	}
}

func TestRepeatGuard_FirstAndLaterFormsDiffer(t *testing.T) {
	g := NewRepeatGuard(RepeatGuardConfig{})
	first, later := "", ""
	for i := 0; i < 8; i++ {
		if r := g.RecordStep([]llm.ContentBlock{gb("Grep", map[string]any{"pattern": "x"})}); r != "" {
			if i < 3 {
				first = r
			} else {
				later = r
			}
		}
	}
	if first == "" || later == "" || first == later {
		t.Fatal("first and later threshold reminders must differ")
	}
	if strings.Contains(first, "- tool:") {
		t.Fatal("first threshold must stay generic (no tool name)")
	}
	if !strings.Contains(later, "- tool: Grep") {
		t.Fatal("later threshold must name the tool")
	}
}
