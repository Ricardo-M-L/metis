package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRecoveredErrorPlansCache_ReusesSpinnerOnlyFrames(t *testing.T) {
	m := newSlashTestModel(t)
	now := time.Now()
	m.messages = []Message{{Role: "user", Content: "retry it", Timestamp: now}}
	m.toolEvents = []ToolEvent{
		{
			ID: "failed", Kind: "result", ToolName: "Bash", IsError: true,
			Input:  map[string]any{"command": "git fetch", "description": "Fetch repository metadata"},
			Output: "temporary EOF", StartTime: now.Add(time.Millisecond),
		},
		{
			ID: "succeeded", Kind: "result", ToolName: "Bash",
			Input:  map[string]any{"command": "git fetch", "description": "Fetch repository metadata"},
			Output: "ok", StartTime: now.Add(2 * time.Millisecond),
		},
	}

	first := m.cachedRecoveredErrorPlans(m.timeline())
	firstPlan := first[&m.toolEvents[0]]
	if firstPlan == nil {
		t.Fatal("fixture did not produce a recovered-error plan")
	}

	// Elapsed time and spinner glyphs change on every frame, but historical
	// messages/tools do not. The exact plan pointer must therefore be reused.
	m.spinnerStartedAt = m.spinnerStartedAt.Add(-time.Second)
	second := m.cachedRecoveredErrorPlans(m.timeline())
	if second[&m.toolEvents[0]] != firstPlan {
		t.Fatal("spinner-only frame rebuilt recovered-error plans")
	}

	// EventToolResult mutates an existing start row in place. Model that
	// transition by taking away the later success: the cheap cache key must
	// notice resultCount changed and invalidate the old recovery evidence.
	m.toolEvents[1].Kind = "start"
	third := m.cachedRecoveredErrorPlans(m.timeline())
	if third[&m.toolEvents[0]] != nil {
		t.Fatal("cache retained recovery after the later success became in-flight")
	}
}

func TestRecoveredErrorPlansCache_InvalidatesSameSizedSessionReplacement(t *testing.T) {
	m := newSlashTestModel(t)
	now := time.Now()
	m.messages = []Message{{Role: "user", Content: "one", Timestamp: now}}
	m.toolEvents = []ToolEvent{{
		ID: "partial", Kind: "result", ToolName: "Bash", IsError: true,
		Input:  map[string]any{"command": "npx skills add x"},
		Output: "Installed 1 skill\ncommand exceeded timeout 30s", StartTime: now.Add(time.Millisecond),
	}}
	first := m.cachedRecoveredErrorPlans(m.timeline())
	if first[&m.toolEvents[0]] == nil {
		t.Fatal("fixture did not produce a partial-completion plan")
	}

	// Session switching can replace both slices with the same counts. Endpoint
	// identities in the key prevent plans (whose keys are old element pointers)
	// from being silently reused for the new session.
	m.messages = append([]Message(nil), Message{Role: "user", Content: "two", Timestamp: now.Add(time.Hour)})
	m.toolEvents = append([]ToolEvent(nil), ToolEvent{
		ID: "plain-error", Kind: "result", ToolName: "Bash", IsError: true,
		Input: map[string]any{"command": "false"}, Output: "exit status 2", StartTime: now.Add(time.Hour + time.Millisecond),
	})
	second := m.cachedRecoveredErrorPlans(m.timeline())
	if second[&m.toolEvents[0]] != nil {
		t.Fatal("same-sized session replacement reused the previous session's plan")
	}
}

// This benchmark targets the v0.4.12 regression directly: busy multi-agent
// transcripts can contain many large transient errors, while the spinner asks
// for a new frame every 40ms. The cached sub-benchmark is the steady-state path
// after the first frame; uncached documents the work we no longer repeat.
func BenchmarkRecoveredErrorPlans_BusyTranscript(b *testing.B) {
	m := newCachedScrollModel(120, 40)
	base := time.Now()
	for turn := 0; turn < 200; turn++ {
		m.messages = append(m.messages, Message{
			Role: "user", Content: fmt.Sprintf("turn %d", turn),
			Timestamp: base.Add(time.Duration(turn*10) * time.Millisecond),
		})
		command := fmt.Sprintf("git fetch origin branch-%d", turn)
		m.toolEvents = append(m.toolEvents,
			ToolEvent{
				ID: fmt.Sprintf("failed-%d", turn), Kind: "result", ToolName: "Bash", IsError: true,
				Input:     map[string]any{"command": command, "description": fmt.Sprintf("Fetch branch-%d metadata", turn)},
				Output:    strings.Repeat("remote: temporary transport EOF\n", 128),
				StartTime: base.Add(time.Duration(turn*10+1) * time.Millisecond),
			},
			ToolEvent{
				ID: fmt.Sprintf("ok-%d", turn), Kind: "result", ToolName: "Bash",
				Input:  map[string]any{"command": command, "description": fmt.Sprintf("Fetch branch-%d metadata", turn)},
				Output: "ok", StartTime: base.Add(time.Duration(turn*10+2) * time.Millisecond),
			},
		)
	}
	merged := m.timeline()

	b.Run("uncached", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = recoveredErrorPlans(merged)
		}
	})
	b.Run("cached", func(b *testing.B) {
		_ = m.cachedRecoveredErrorPlans(merged)
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = m.cachedRecoveredErrorPlans(merged)
		}
	})
}
