package webui

import (
	"strings"

	"github.com/Ricardo-M-L/metis/internal/session"
)

// traceToolMatchRank prefers immutable call IDs. Legacy fallbacks must stay
// inside a turn and cannot cross known invocation or provider-ID boundaries.
// Callers refuse tied candidates rather than picking an arbitrary same-name tool.
func traceToolMatchRank(a, b session.TraceEvent) int {
	if a.Turn != b.Turn || (a.TraceInvocationID != "" && b.TraceInvocationID != "" && a.TraceInvocationID != b.TraceInvocationID) {
		return 0
	}
	if a.TraceCallID != "" && b.TraceCallID != "" {
		if a.TraceCallID == b.TraceCallID {
			return 4
		}
		return 0
	}
	aID, bID := a.ToolUseID, b.ToolUseID
	if bID == "" && b.Kind == "tool_result" {
		bID = b.ParentID
	}
	if aID != "" && bID != "" {
		if aID == bID {
			return 3
		}
		return 0
	}
	if a.ToolName != "" && a.ToolName == b.ToolName {
		return 2
	}
	if a.ToolName == "" && aID == "" {
		return 1
	}
	return 0
}

func inheritTraceToolIdentity(dst *session.TraceEvent, src session.TraceEvent) {
	if dst.ToolName == "" {
		dst.ToolName = src.ToolName
	}
	if dst.ToolUseID == "" {
		dst.ToolUseID = src.ToolUseID
	}
	if dst.TraceInvocationID == "" {
		dst.TraceInvocationID = src.TraceInvocationID
	}
	if dst.TraceCallID == "" {
		dst.TraceCallID = src.TraceCallID
	}
}

// coalesceTraceToolArgs preserves the input tree's order. A missing start is
// synthesized at the first argument fragment, never at the end of the session.
// A result seals that stream even when no start was recorded (old ToolSearch
// fast paths), so reused provider IDs cannot concatenate later calls into it.
func coalesceTraceToolArgs(nodes []session.TracedNode) []session.TracedNode {
	type partial struct {
		node       session.TracedNode
		text       strings.Builder
		consumed   bool
		resultSeen bool
	}
	type slot struct {
		node    session.TracedNode
		partial *partial
	}
	active := make(map[*partial]struct{})
	find := func(ev session.TraceEvent) *partial {
		var best *partial
		bestRank, tied := 0, false
		for candidate := range active {
			rank := traceToolMatchRank(candidate.node.Event, ev)
			if rank > bestRank {
				best, bestRank, tied = candidate, rank, false
			} else if rank > 0 && rank == bestRank {
				tied = true
			}
		}
		if tied {
			return nil
		}
		return best
	}
	slots := make([]slot, 0, len(nodes))
	for _, node := range nodes {
		ev := node.Event
		if ev.Kind == "tool_args" || ev.Kind == "tool_args_delta" {
			p := find(ev)
			if p == nil {
				p = &partial{node: node}
				active[p] = struct{}{}
				slots = append(slots, slot{partial: p})
			}
			p.text.WriteString(ev.Text)
			inheritTraceToolIdentity(&p.node.Event, ev)
			continue
		}
		if ev.Kind == "tool_start" || ev.Kind == "tool_result" {
			if p := find(ev); p != nil {
				delete(active, p)
				inheritTraceToolIdentity(&node.Event, p.node.Event)
				if ev.Kind == "tool_start" {
					p.consumed = true
					current := strings.TrimSpace(ev.Text)
					if p.text.Len() > 0 && (current == "" || current == "null" || current == "{}") {
						node.Event.Text = p.text.String()
					}
				} else {
					p.resultSeen = true
					inheritTraceToolIdentity(&p.node.Event, node.Event)
				}
			}
		}
		slots = append(slots, slot{node: node})
	}
	out := make([]session.TracedNode, 0, len(slots))
	for _, slot := range slots {
		if p := slot.partial; p != nil {
			if p.consumed || (p.text.Len() == 0 && !p.resultSeen) {
				continue
			}
			node := p.node
			node.Event.Kind, node.Event.Text = "tool_start", p.text.String()
			out = append(out, node)
		} else {
			out = append(out, slot.node)
		}
	}
	return out
}
