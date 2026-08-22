package builtin

// trace_tools.go — model-visible trajectory tools (harness parity:
// session_event_read / session_event_search / session_trace). They
// read the process-wide trace store installed by
// runtime.InstallTrace; without it, every tool degrades to a clean
// "unavailable" result.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

var (
	traceStoreMu      sync.RWMutex
	currentTraceStore *session.TraceStore
)

// SetTraceStore installs the process-wide trace store (runtime wiring).
func SetTraceStore(s *session.TraceStore) {
	traceStoreMu.Lock()
	defer traceStoreMu.Unlock()
	currentTraceStore = s
}

// CurrentTraceStore returns the active store, or nil when tracing is
// disabled for this process.
func CurrentTraceStore() *session.TraceStore {
	traceStoreMu.RLock()
	defer traceStoreMu.RUnlock()
	return currentTraceStore
}

// resolveSessionID picks the session to operate on: the tool's explicit
// sessionId wins, then the runtime's active session, then empty.
func resolveSessionID(in map[string]any) string {
	if sid, _ := in["sessionId"].(string); sid != "" {
		return sid
	}
	return tasks.CurrentSessionID()
}

// traceEventToCompact renders one event on a single line (for list views).
func traceEventToCompact(ev session.TraceEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%-5d %-4d %-18s", ev.Sequence, ev.Turn, ev.Kind)
	if ev.ToolName != "" {
		fmt.Fprintf(&b, " %s", ev.ToolName)
	}
	text := oneLine(ev.Text, 60)
	if text != "" {
		fmt.Fprintf(&b, " | %s", text)
	}
	if ev.IsError {
		b.WriteString(" [ERROR]")
	}
	return b.String()
}

func oneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	// Cut on a sensible boundary to avoid splitting multi-byte runes.
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// SessionEventRead reads trace events for a session (or one event by ID).
type SessionEventRead struct {
	tools.BaseTool
	gate *permission.Gate
}

func NewSessionEventRead(gate *permission.Gate) SessionEventRead {
	return SessionEventRead{gate: gate}
}

func (SessionEventRead) Name() string { return "SessionEventRead" }

func (SessionEventRead) Description() string {
	return "Read trajectory events for a session. Without eventId, returns the session's event log (most recent first); with eventId, returns the single event plus its children. Use this to inspect what the agent actually did in an earlier session: tool calls, durations, errors, token usage."
}

func (SessionEventRead) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"sessionId": map[string]any{"type": "string", "description": "Session to read (defaults to the current session)"},
			"eventId":   map[string]any{"type": "string", "description": "Optional event UUID or tool_use_id - return that event plus its children"},
			"limit":     map[string]any{"type": "integer", "description": "Max events to return (default 100, max 1000)"},
		},
	}
}

func (SessionEventRead) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (r SessionEventRead) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := r.gate.Check(context.Background(), "SessionEventRead", "")
	return mapDecision(d), src
}

func (r SessionEventRead) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	store := CurrentTraceStore()
	if store == nil {
		return errResult("SessionEventRead: session tracing is not enabled for this process"), nil
	}
	sid := resolveSessionID(in)
	if sid == "" {
		return errResult("SessionEventRead: no sessionId given and no active session"), nil
	}
	limit := 100
	if n, ok := in["limit"].(float64); ok && int(n) > 0 {
		limit = int(n)
		if limit > 1000 {
			limit = 1000
		}
	}
	eventID, _ := in["eventId"].(string)

	// Single-event view: the requested event first (never reordered or
	// truncated away), then its direct children capped by limit.
	// eventId matches either the event's UUID or its tool_use_id -
	// children reference parents by tool_use_id (ParentID/SubAgentOf),
	// so a tool_use_id is the address that makes the tree walkable.
	if eventID != "" {
		all := store.Events(sid)
		var matched session.TraceEvent
		found := false
		for _, ev := range all {
			if ev.ID == eventID || (ev.ToolUseID != "" && ev.ToolUseID == eventID) {
				matched = ev
				found = true
				break
			}
		}
		if !found {
			return errResult(fmt.Sprintf("SessionEventRead: event %q not found in session %s", eventID, sid)), nil
		}
		parentKeys := map[string]bool{matched.ID: true}
		if matched.ToolUseID != "" {
			parentKeys[matched.ToolUseID] = true
		}
		raw, _ := json.MarshalIndent(matched, "  ", "  ")
		var b strings.Builder
		b.WriteString(traceEventToCompact(matched) + "\n\n" + string(raw))
		n := 0
		for _, ev := range all {
			if ev.ID == matched.ID {
				continue
			}
			if (ev.ParentID != "" && parentKeys[ev.ParentID]) || (ev.SubAgentOf != "" && parentKeys[ev.SubAgentOf]) {
				if n >= limit {
					b.WriteString("\n  ... (more children truncated)")
					break
				}
				n++
				b.WriteString("\n  child: " + traceEventToCompact(ev))
			}
		}
		return &tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
	}

	// List view: newest first, capped.
	events := store.Events(sid)
	sort.Slice(events, func(i, j int) bool { return events[i].Sequence > events[j].Sequence })
	if len(events) > limit {
		events = events[:limit]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d event(s) from session %s (newest first):\n", len(events), sid)
	for _, ev := range events {
		b.WriteString("  " + traceEventToCompact(ev) + "\n")
	}
	return &tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}

// SessionEventSearch does full-text search over recorded trajectory events.
type SessionEventSearch struct {
	tools.BaseTool
	gate *permission.Gate
}

func NewSessionEventSearch(gate *permission.Gate) SessionEventSearch {
	return SessionEventSearch{gate: gate}
}

func (SessionEventSearch) Name() string { return "SessionEventSearch" }

func (SessionEventSearch) Description() string {
	return "Full-text search over recorded trajectory events (tool outputs, errors, token usage across all sessions). Terms are AND-ed, case-insensitive. Use it to answer questions like 'when did I see X error' or 'which session used the RunCode tool'."
}

func (SessionEventSearch) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"query"},
		"properties": map[string]any{
			"query":     map[string]any{"type": "string", "description": "Search terms (space-separated; all must match)"},
			"sessionId": map[string]any{"type": "string", "description": "Restrict search to one session (default: all sessions)"},
			"limit":     map[string]any{"type": "integer", "description": "Max results (default 20, max 100)"},
		},
	}
}

func (SessionEventSearch) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (r SessionEventSearch) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := r.gate.Check(context.Background(), "SessionEventSearch", "")
	return mapDecision(d), src
}

func (r SessionEventSearch) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	store := CurrentTraceStore()
	if store == nil {
		return errResult("SessionEventSearch: session tracing is not enabled for this process"), nil
	}
	query, _ := in["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return errResult("SessionEventSearch: query is required"), nil
	}
	sid, _ := in["sessionId"].(string)
	limit := 20
	if n, ok := in["limit"].(float64); ok && int(n) > 0 {
		limit = int(n)
		if limit > 100 {
			limit = 100
		}
	}
	results, err := store.Search(query, sid, limit)
	if err != nil {
		return errResult("SessionEventSearch: " + err.Error()), nil
	}
	if len(results) == 0 {
		return &tools.Result{Output: "(no matching events)"}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es) for %q:\n", len(results), query)
	for _, ev := range results {
		fmt.Fprintf(&b, "  [%s] ", ev.SessionID)
		b.WriteString(traceEventToCompact(ev) + "\n")
	}
	return &tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}

// SessionTrace renders the session's event tree (parents followed by
// nested children) — the agent's trajectory as a hierarchy.
type SessionTrace struct {
	tools.BaseTool
	gate *permission.Gate
}

func NewSessionTrace(gate *permission.Gate) SessionTrace {
	return SessionTrace{gate: gate}
}

func (SessionTrace) Name() string { return "SessionTrace" }

func (SessionTrace) Description() string {
	return "Render the trajectory of a session as a tree: root events (text, tool starts) with their nested children (tool results, sub-agent activity). Shows the full causal structure of what happened, unlike the flat log."
}

func (SessionTrace) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"sessionId": map[string]any{"type": "string", "description": "Session to trace (defaults to the current session)"},
			"maxDepth":  map[string]any{"type": "integer", "description": "Cap nesting depth (default 8)"},
		},
	}
}

func (SessionTrace) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (r SessionTrace) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := r.gate.Check(context.Background(), "SessionTrace", "")
	return mapDecision(d), src
}

func (r SessionTrace) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	store := CurrentTraceStore()
	if store == nil {
		return errResult("SessionTrace: session tracing is not enabled for this process"), nil
	}
	sid := resolveSessionID(in)
	if sid == "" {
		return errResult("SessionTrace: no sessionId given and no active session"), nil
	}
	maxDepth := 8
	if n, ok := in["maxDepth"].(float64); ok && int(n) > 0 {
		maxDepth = int(n)
	}

	nodes := store.Trace(sid)
	if len(nodes) == 0 {
		return &tools.Result{Output: fmt.Sprintf("(no trajectory recorded for session %s)", sid)}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Trajectory of session %s:\n", sid)
	for _, n := range nodes {
		indent := strings.Repeat("  ", n.Depth)
		if n.Depth > maxDepth {
			continue
		}
		label := n.Event.Kind
		if n.Event.ToolName != "" {
			label += ":" + n.Event.ToolName
		}
		text := oneLine(n.Event.Text, 80)
		if text != "" {
			label += " | " + text
		}
		if n.Event.IsError {
			label += " [ERROR]"
		}
		if n.Event.ElapsedMs > 0 {
			label += fmt.Sprintf(" (%dms)", n.Event.ElapsedMs)
		}
		switch n.Event.Kind {
		case "text":
			fmt.Fprintf(&b, "%s▸ %s\n", indent, text)
		case "tool_start":
			fmt.Fprintf(&b, "%s⚙ %s\n", indent, label)
		case "tool_result":
			fmt.Fprintf(&b, "%s✓ %s\n", indent, label)
		case "error":
			fmt.Fprintf(&b, "%s✗ %s\n", indent, label)
		default:
			fmt.Fprintf(&b, "%s· %s\n", indent, label)
		}
	}
	return &tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}
