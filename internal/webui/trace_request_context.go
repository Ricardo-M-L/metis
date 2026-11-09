package webui

import "github.com/Ricardo-M-L/metis/internal/session"

// The independently returned snapshot remains available on every page. Its
// recorded turn and timestamp are never moved ahead of the first user row in
// storage; only the UI chooses where to present this request context panel.
func splitInitialTraceContext(nodes []session.TracedNode) ([]session.TracedNode, *session.TraceRequestContext) {
	initial := &session.TraceRequestContext{Available: false, Source: "unavailable", Tools: []session.TraceRequestTool{},
		Limitation: "The first actual request context was not recorded for this session. Saved header prompts cannot establish its system or tools."}
	out := make([]session.TracedNode, 0, len(nodes))
	for _, node := range nodes {
		ev := node.Event
		if ev.Kind != "request_context" {
			out = append(out, node)
			continue
		}
		if initial.Available || ev.RequestContext == nil || node.Depth != 0 || ev.SubAgentOf != "" || ev.ParentID != "" || ev.TraceParentInvocationID != "" {
			continue
		}
		cp := *ev.RequestContext
		initial = &cp
		if initial.Turn == 0 {
			initial.Turn = ev.Turn
		}
		if initial.CapturedAt.IsZero() {
			initial.CapturedAt = ev.TS
		}
		if initial.Tools == nil {
			initial.Tools = []session.TraceRequestTool{}
		}
	}
	return out, initial
}
