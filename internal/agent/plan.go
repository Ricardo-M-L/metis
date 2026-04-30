package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToolCall lives in event.go to avoid duplication.

// Plan is the output of a plan-mode turn: text description + tool calls.
type Plan struct {
	Text      string     `json:"text"`
	ToolCalls []ToolCall `json:"-"`
	Summary   string     `json:"summary"`
}

// Render returns a formatted markdown plan for user review.
func (p *Plan) Render() string {
	var b strings.Builder
	b.WriteString("## Plan\n\n")
	b.WriteString(p.Text)
	b.WriteString("\n\n### Tool Calls\n\n")
	for i, tc := range p.ToolCalls {
		argsJSON, _ := json.MarshalIndent(tc.Input, "  ", "  ")
		fmt.Fprintf(&b, "%d. **%s**\n\n", i+1, tc.Name)
		b.WriteString("```json\n")
		b.Write(argsJSON)
		b.WriteString("\n```\n\n")
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "**Summary:** %s\n\n", p.Summary)
	b.WriteString("**Proceed?** `y` (yes) / `n` (no) / `a` (always) / `e` (edit)\n")
	return b.String()
}

// CollectToolCalls walks events and assembles the tool call list.
// For Plan Mode: events include EventPlan with ToolCalls populated.
func CollectToolCallsFromEvents(events []Event) []ToolCall {
	var calls []ToolCall
	for _, ev := range events {
		if ev.Kind == EventPlan {
			calls = append(calls, ev.ToolCalls...)
		}
	}
	return calls
}
