//go:build dump

// To preview the new chat-surface rendering visually, run:
//
//	go test -tags dump -v -run TestVisualDump ./internal/tui/
//
// Build tag keeps this out of `go test ./...` so the regular suite stays
// silent. The test forces lipgloss into ANSI256 so escape codes survive
// `go test`'s output capture and the user sees actual colors.

package tui

import (
	"fmt"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/muesli/termenv"
)

func TestVisualDump(t *testing.T) {
	// Force color output even when stdout isn't a TTY (go test redirects).
	lipgloss.SetColorProfile(termenv.ANSI256)

	dump := func(s string) { fmt.Print(s) }

	// Banner separator
	dump("\n────────────── METIS UI PREVIEW ──────────────\n")

	// Welcome banner — was an ASCII block logo before commit 345a087
	// (the `█▀` for S and `█▀▀` for E rendered as Γ/C in some fonts and
	// the user saw "MCTIS"). Today render_welcome.go uses a stylized
	// title; we reproduce the same look here for the dump preview
	// without calling renderWelcomeBanner directly (that needs a full
	// Model with model/gate/sessionID set up).
	dump("\n")
	dump(lipgloss.NewStyle().Foreground(lipgloss.Color("#64b5f6")).Bold(true).Render("  ✻ metis"))
	dump("\n")
	dump(lipgloss.NewStyle().Foreground(lipgloss.Color("#a0a0a0")).Italic(true).Render("  local-first agent CLI · cunning intelligence"))
	dump("\n\n")

	// User input (was "› ", now "❯ ")
	dump(renderMessage(Message{Role: "user", Content: "fix the bug in compaction trigger"}, 80))

	// Assistant text — plain
	dump(renderMessage(Message{Role: "assistant", Content: "Looking at the compaction trigger logic in agent/compact.go."}, 80))

	// Assistant text — markdown (code fence + bold + list)
	dump(renderMessage(Message{Role: "assistant", Content: "Here's the **fix**:\n\n```go\nif usage > threshold {\n    if err := Compact(); err != nil {\n        return err\n    }\n}\n```\n\nKey changes:\n- Wrapped the call in error check\n- Added a *log.Info* trace before compaction"}, 80))

	// Read — in-flight
	dump(renderToolEvent(ToolEvent{
		Kind:     "start",
		ToolName: "Read",
		Input:    map[string]any{"path": "/Users/me/metis/internal/agent/compact.go"},
	}, false))

	// Read — completed (with line-count summary)
	dump(renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Read",
		Input:    map[string]any{"path": "/Users/me/metis/internal/agent/compact.go"},
		Output:   "package agent\n\nimport \"context\"\n\nfunc (l *Loop) Compact(ctx context.Context) error {\n\treturn nil\n}\n",
		Duration: 12 * time.Millisecond,
	}, false))

	// Edit — completed with structured diff (this is the big new pattern)
	dump(renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Edit",
		Input: map[string]any{
			"path":       "internal/agent/compact.go",
			"old_string": "if usage > threshold {\n    return Compact()\n}\n",
			"new_string": "if usage > threshold {\n    log.Info(\"triggering compaction\")\n    if err := Compact(); err != nil {\n        return err\n    }\n}\n",
		},
		Duration: 8 * time.Millisecond,
	}, false))

	// Write — green-only preview
	dump(renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Write",
		Input: map[string]any{
			"path":    "internal/agent/compact_metric.go",
			"content": "package agent\n\nimport \"sync/atomic\"\n\nvar compactCount atomic.Int64\n\nfunc bumpCompactCount() {\n\tcompactCount.Add(1)\n}\n",
		},
		Duration: 4 * time.Millisecond,
	}, false))

	// Bash — first-line summary + preview body
	dump(renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Bash",
		Input:    map[string]any{"command": "go test ./internal/agent/"},
		Output:   "ok  github.com/Ricardo-M-L/metis/internal/agent  2.412s\n? github.com/Ricardo-M-L/metis/internal/agent/transcript [no test files]\nPASS\nok all tests green\nfinal stats: 23 tests, 0 failures\n",
		Duration: 2412 * time.Millisecond,
	}, false))

	// Grep — match-count summary
	dump(renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Grep",
		Input:    map[string]any{"pattern": "Compact"},
		Output:   "internal/agent/compact.go:1\ninternal/agent/compact.go:42\ninternal/agent/loop.go:80\ninternal/agent/loop.go:215",
		Duration: 18 * time.Millisecond,
	}, false))

	// Bash error path (red ✗)
	dump(renderToolEvent(ToolEvent{
		Kind:     "result",
		ToolName: "Bash",
		Input:    map[string]any{"command": "go test ./broken/"},
		Output:   "build failed",
		IsError:  true,
		Duration: 200 * time.Millisecond,
	}, false))

	// Streaming token line (post-30s, mid-stream — the format we just shipped)
	// Note: this is what renderSpinnerStatus produces; we emulate the line.
	dump("\n  ⏺ ")
	dump(lipgloss.NewStyle().Foreground(lipgloss.Color("#909090")).Render("Pondering"))
	dump(lipgloss.NewStyle().Foreground(lipgloss.Color("#606060")).Render(" (12s · ↓ 3.1k tokens · thought for 2s)"))
	dump("\n")

	// Turn-end thought summary
	dump(renderMessage(Message{
		Role:    "thought-summary",
		Content: fmt.Sprintf("%s for %s", "Cogitated", formatTurnDuration(78*time.Second)),
	}, 80))

	// Recap (structural, derived from the turn's tool events)
	mockEvents := []ToolEvent{
		{Kind: "result", ToolName: "Read", Input: map[string]any{"path": "compact.go"}},
		{Kind: "result", ToolName: "Edit", Input: map[string]any{"path": "compact.go"}},
		{Kind: "result", ToolName: "Write", Input: map[string]any{"path": "compact_metric.go"}},
		{Kind: "result", ToolName: "Bash", Input: map[string]any{"command": "go test ./internal/agent/"}},
	}
	if recap := buildTurnRecap(mockEvents); recap != "" {
		dump(renderMessage(Message{Role: "recap", Content: recap}, 80))
	}

	// Compaction banner
	dump(renderMessage(Message{
		Role:    "compaction",
		Content: "context compacted: 47 → 12 messages",
	}, 80))

	// Final ANSI reset so the user's prompt color isn't stuck
	dump("\033[0m")
	dump("──────────────────────────────────────────────\n\n")
}
