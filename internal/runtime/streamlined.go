// Package runtime — streamlined (distillation-resistant) output mode.
//
// Direct port of claude-code's utils/streamlinedTransform.ts. Goal: when
// metis is invoked non-interactively (`metis run`, scripted batches,
// CI), make the output stream useless as training data without
// breaking it for the legitimate caller.
//
// What gets stripped:
//
//   - Thinking content (omitted entirely — gold for distillation)
//   - Per-tool call detail (collapsed into cumulative summaries)
//   - Tool error stack traces (kept as terse "[tool error]")
//
// What stays intact:
//
//   - The model's actual text answer (the user paid for this)
//   - Cumulative tool counts between text messages (so the user can
//     still tell "agent did stuff", just not exactly what)
//   - Errors (so scripts can detect failure)
//
// Counts reset every time a text chunk arrives — same as CC's
// behavior. Pattern: tool calls accumulate silently, then when the
// model finally says something, we flush the summary as a stderr line
// AHEAD of the text, then start counting again from zero.
package runtime

import (
	"fmt"
	"strings"
)

// streamlinedTools categorizes tool names into the 5 buckets CC uses.
// Tool names match metis's builtin registry; unknown tools fall into
// "other" — same fail-open behavior as CC for MCP / plugin tools.
var streamlinedTools = map[string]string{
	// search bucket
	"Grep":      "search",
	"Glob":      "search",
	"WebSearch": "search",
	"WebFetch":  "search",
	"WebBrowse": "search",
	"LSP":       "search",
	// read bucket
	"Read": "read",
	"LS":   "read",
	// write bucket
	"Write":        "write",
	"Edit":         "write",
	"NotebookEdit": "write",
	// command bucket
	"Bash": "command",
	"Git":  "command",
}

func categorizeStreamlined(toolName string) string {
	if c, ok := streamlinedTools[toolName]; ok {
		return c
	}
	return "other"
}

// StreamlinedAccumulator tracks cumulative tool-call counts between
// text messages. Reset whenever text appears — that's the boundary
// CC uses to flush summaries.
type StreamlinedAccumulator struct {
	searches int
	reads    int
	writes   int
	commands int
	other    int
}

// AccumulateTool bumps the appropriate counter.
func (s *StreamlinedAccumulator) AccumulateTool(toolName string) {
	switch categorizeStreamlined(toolName) {
	case "search":
		s.searches++
	case "read":
		s.reads++
	case "write":
		s.writes++
	case "command":
		s.commands++
	default:
		s.other++
	}
}

// Reset zeroes all counters. Called after a Summary() flush.
func (s *StreamlinedAccumulator) Reset() {
	*s = StreamlinedAccumulator{}
}

// Empty reports whether anything has been accumulated. Cheap check
// before the caller decides whether to format + print a summary.
func (s *StreamlinedAccumulator) Empty() bool {
	return s.searches == 0 && s.reads == 0 && s.writes == 0 && s.commands == 0 && s.other == 0
}

// Summary returns the user-facing one-liner like
// "Searched 3 patterns, read 2 files, ran 1 command". Empty string
// when nothing accumulated. Phrasing mirrors CC's getToolSummaryText
// down to the singular/plural toggling.
func (s *StreamlinedAccumulator) Summary() string {
	if s.Empty() {
		return ""
	}
	var parts []string
	if s.searches > 0 {
		parts = append(parts, fmt.Sprintf("searched %d %s", s.searches, plural("pattern", s.searches)))
	}
	if s.reads > 0 {
		parts = append(parts, fmt.Sprintf("read %d %s", s.reads, plural("file", s.reads)))
	}
	if s.writes > 0 {
		parts = append(parts, fmt.Sprintf("wrote %d %s", s.writes, plural("file", s.writes)))
	}
	if s.commands > 0 {
		parts = append(parts, fmt.Sprintf("ran %d %s", s.commands, plural("command", s.commands)))
	}
	if s.other > 0 {
		parts = append(parts, fmt.Sprintf("%d other %s", s.other, plural("tool", s.other)))
	}
	out := strings.Join(parts, ", ")
	if len(out) > 0 {
		out = strings.ToUpper(out[:1]) + out[1:]
	}
	return out
}

func plural(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}
