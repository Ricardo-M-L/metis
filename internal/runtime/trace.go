package runtime

// trace.go — process-wide agent event trace wiring.
//
// Runtime assembles the trace once per process boot:
//
//	adapter, _ := runtime.InstallTrace(traceDir)
//	...
//	adapter.SetSession(sid)   // on every session switch (rebindSession)
//	...
//	runtime.FlushTrace()      // on shutdown (defer in main)
//
// InstallTrace registers:
//   - agent.SetTraceHook(adapter.OnEvent)  — capture every event
//   - builtin.SetTraceStore(store)         — session_event_* tools
//
// The adapter aggregates adjacent streaming text/reasoning deltas into
// typed events, tracks per-session turn numbers, and preserves sub-agent
// parents so the stored trace forms a spawn tree.

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

// TraceAdapter converts agent.Events into session.TraceEvents for the
// active session, buffering typed streaming bursts and tracking turn starts.
type TraceAdapter struct {
	store *session.TraceStore

	mu         sync.Mutex
	sessionID  string
	turnActive bool
	burstKind  string
	burstBuf   strings.Builder
}

const redactedThinkingPlaceholder = "Reasoning redacted by provider"

// NewTraceAdapter wraps a TraceStore.
func NewTraceAdapter(store *session.TraceStore) *TraceAdapter {
	return &TraceAdapter{store: store}
}

// SetSession switches the active trace session. Called on every
// /new /resume /branch and web-session switch, so each channel of
// events lands in its own JSONL file.
func (a *TraceAdapter) SetSession(sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.flushBurstLocked()
	a.sessionID = sid
	a.turnActive = false // next event opens a fresh turn
}

// OnEvent adapts one agent event. Installed via agent.SetTraceHook —
// called synchronously on every emitting loop goroutine (main + sub-agents).
// RecordUserMessage stamps the human turn input into the active session's
// trace. It gives the trajectory a USER row and anchors per-turn
// time-to-first-token (first assistant text minus the user message). The
// adapter owns turn numbering, so this shares its lock and turn state.
func RecordUserMessage(sessionID, text string) {
	adapterMu.RLock()
	a := currentTraceAdapter
	adapterMu.RUnlock()
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil || sessionID == "" || strings.TrimSpace(text) == "" {
		return
	}
	a.ensureTurnLocked(sessionID)
	a.appendLocked(sessionID, "user", agent.Event{Kind: agent.EventChannelInbound}, TraceExtra{Text: strings.TrimSpace(text)})
}

func (a *TraceAdapter) OnEvent(ev agent.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.store == nil || a.sessionID == "" {
		return
	}

	// Adjacent streaming deltas are aggregated into typed bursts. A burst
	// ends when its type changes (thinking -> text) or at the first
	// non-delta event, preserving the provider's trajectory order.
	sid := a.sessionID
	switch ev.Kind {
	case agent.EventTextDelta:
		a.bufferDeltaLocked(sid, "text", ev.TextDelta)
		return
	case agent.EventStreamStart, agent.EventStreamEnd:
		return // transport noise — not part of the agent's trajectory
	case agent.EventToolStart:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		inputJSON, _ := json.Marshal(ev.ToolInput)
		parent := ev.SubAgentParentID
		a.appendLocked(sid, "tool_start", ev, TraceExtra{
			ToolName:  ev.ToolName,
			ToolUseID: ev.ToolUseID,
			Text:      string(inputJSON),
			ParentID:  parent,
		})
	case agent.EventToolResult:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		var out string
		var isErr bool
		if ev.ToolResult != nil {
			out = ev.ToolResult.Output
			isErr = ev.ToolResult.IsError
		}
		a.appendLocked(sid, "tool_result", ev, TraceExtra{
			ToolName:  ev.ToolName,
			ToolUseID: ev.ToolUseID,
			Text:      out,
			IsError:   isErr,
			ElapsedMs: ev.Elapsed.Milliseconds(),
			ParentID:  ev.ToolUseID,
		})
	case agent.EventTurnEnd:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		a.appendLocked(sid, "turn_end", ev, TraceExtra{})
	case agent.EventLoopDone:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		a.appendLocked(sid, "loop_done", ev, TraceExtra{Text: ev.StopReason})
		a.turnActive = false
	case agent.EventTokens:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		a.appendLocked(sid, "tokens", ev, TraceExtra{Text: fmt.Sprintf(
			"input=%d output=%d cache_write=%d cache_read=%d",
			ev.InputTokens, ev.OutputTokens, ev.CacheCreationInputTokens, ev.CacheReadInputTokens)})
	case agent.EventError:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		a.appendLocked(sid, "error", ev, TraceExtra{
			Text: fmt.Sprint(ev.Err), IsError: true,
		})
	case agent.EventPermissionRequest:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		a.appendLocked(sid, "permission_request", ev, TraceExtra{
			ToolName: ev.PermissionTool,
			Text:     ev.PermissionReason,
		})
	case agent.EventSubAgentStart, agent.EventSubAgentEnd, agent.EventSubAgentProgress:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		k := eventKindLabel(ev.Kind)
		a.appendLocked(sid, k, ev, TraceExtra{
			ToolUseID: ev.ToolUseID,
			ParentID:  ev.SubAgentParentID,
			Text:      ev.Info,
		})
	case agent.EventPlan:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		a.appendLocked(sid, "plan", ev, TraceExtra{Text: ev.Info})
	case agent.EventRateLimitHit:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		a.appendLocked(sid, "rate_limit", ev, TraceExtra{Text: ev.Info})
	case agent.EventModelFallback:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		a.appendLocked(sid, "model_fallback", ev, TraceExtra{Text: ev.Info})
	case agent.EventContextWarn, agent.EventContextCompacted, agent.EventCompactionStart, agent.EventCompactionProgress, agent.EventCompactionEnd:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		a.appendLocked(sid, eventKindLabel(ev.Kind), ev, TraceExtra{Text: ev.Info})
	case agent.EventThinkingDelta:
		a.bufferDeltaLocked(sid, "thinking", ev.TextDelta)
		return
	case agent.EventRedactedThinking:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		// The event carries opaque provider ciphertext in TextDelta solely
		// for transcript round-tripping. Trace/UI surfaces must never store it.
		a.appendLocked(sid, "thinking_redacted", ev, TraceExtra{Text: redactedThinkingPlaceholder})
		return
	case agent.EventToolArgsDelta:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		a.appendLocked(sid, "tool_args", ev, TraceExtra{
			ToolName:  ev.ToolName,
			ToolUseID: ev.ToolUseID,
			Text:      ev.TextDelta,
		})
	case agent.EventAskUser:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		a.appendLocked(sid, "ask_user", ev, TraceExtra{Text: ev.AskUserQuestion})
	case agent.EventChannelInbound, agent.EventChannelSent, agent.EventHookFired,
		agent.EventDreamingStart, agent.EventDreamingProgress, agent.EventDreamingEnd:
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		a.appendLocked(sid, eventKindLabel(ev.Kind), ev, TraceExtra{Text: ev.Info})
	default:
		// EventInfo and anything else lands as an "info" event.
		a.ensureTurnLocked(sid)
		a.flushBurstLocked()
		a.appendLocked(sid, "info", ev, TraceExtra{Text: ev.Info})
	}
}

// TraceExtra carries the mapped fields into a TraceEvent.
type TraceExtra struct {
	ToolName  string
	ToolUseID string
	Text      string
	IsError   bool
	ElapsedMs int64
	ParentID  string
}

func (a *TraceAdapter) appendLocked(sid, kind string, ev agent.Event, x TraceExtra) {
	te := &session.TraceEvent{
		SessionID: sid,
		Kind:      kind,
		ToolName:  x.ToolName,
		ToolUseID: x.ToolUseID,
		Text:      truncateTrace(x.Text, 2000),
		IsError:   x.IsError,
		ElapsedMs: x.ElapsedMs,
		ParentID:  x.ParentID,
	}
	if ev.SubAgentParentID != "" && x.ParentID == "" {
		te.SubAgentOf = ev.SubAgentParentID
	}
	if err := a.store.Append(te); err != nil {
		log.Printf("trace: append event: %v", err)
	}
}

// ensureTurnLocked opens a new turn if one isn't active.
func (a *TraceAdapter) ensureTurnLocked(sid string) {
	if a.turnActive {
		return
	}
	a.store.NextTurn(sid)
	a.turnActive = true
}

// bufferDeltaLocked appends a provider delta to the active typed burst,
// settling the previous burst first when the provider changes type.
func (a *TraceAdapter) bufferDeltaLocked(sid, kind, delta string) {
	if delta == "" {
		return
	}
	a.ensureTurnLocked(sid)
	if a.burstKind != "" && a.burstKind != kind {
		a.flushBurstLocked()
	}
	a.burstKind = kind
	a.burstBuf.WriteString(delta)
}

// flushBurstLocked emits the accumulated typed delta burst as one event.
func (a *TraceAdapter) flushBurstLocked() {
	if a.burstBuf.Len() == 0 {
		a.burstKind = ""
		return
	}
	kind := a.burstKind
	text := strings.TrimSpace(a.burstBuf.String())
	a.burstBuf.Reset()
	a.burstKind = ""
	if text == "" || a.sessionID == "" {
		return
	}
	if err := a.store.Append(&session.TraceEvent{
		SessionID: a.sessionID,
		Kind:      kind,
		Text:      truncateTrace(text, 2000),
	}); err != nil {
		log.Printf("trace: append %s event: %v", kind, err)
	}
}

func eventKindLabel(k agent.EventKind) string {
	switch k {
	case agent.EventSubAgentStart:
		return "subagent_start"
	case agent.EventSubAgentEnd:
		return "subagent_end"
	case agent.EventSubAgentProgress:
		return "subagent_progress"
	case agent.EventContextWarn:
		return "context_warn"
	case agent.EventContextCompacted:
		return "context_compacted"
	case agent.EventCompactionStart:
		return "compaction_start"
	case agent.EventCompactionProgress:
		return "compaction_progress"
	case agent.EventCompactionEnd:
		return "compaction_end"
	case agent.EventChannelInbound:
		return "channel_inbound"
	case agent.EventChannelSent:
		return "channel_sent"
	case agent.EventHookFired:
		return "hook_fired"
	case agent.EventDreamingStart:
		return "dreaming_start"
	case agent.EventDreamingProgress:
		return "dreaming_progress"
	case agent.EventDreamingEnd:
		return "dreaming_end"
	}
	return "info"
}

func truncateTrace(s string, n int) string {
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "...(truncated)"
}

// InstallTrace builds the store, wires the hook, and exposes the store
// to the session_event_* tools. Returns the adapter for session
// switching. A nil traceDir disables tracing cleanly.
func InstallTrace(traceDir string) *TraceAdapter {
	if traceDir == "" {
		return nil
	}
	store, err := session.NewTraceStore(traceDir)
	if err != nil {
		log.Printf("runtime: InstallTrace: %v (tracing disabled)", err)
		return nil
	}
	builtin.SetTraceStore(store)
	adapter := NewTraceAdapter(store)
	SetTraceAdapter(adapter)
	agent.SetTraceHook(adapter.OnEvent)
	return adapter
}

// FlushTrace persists any buffered trace writes. Safe to call more than
// once; a nil store is a no-op. Intended for `defer` in main.
func FlushTrace() {
	adapter := CurrentTraceAdapter()
	if adapter == nil || adapter.store == nil {
		return
	}
	_ = adapter.store.Sync()
}

// currentTraceAdapter is the process-global adapter installed by
// InstallTrace.
var (
	adapterMu           sync.RWMutex
	currentTraceAdapter *TraceAdapter
)

// SetTraceAdapter records the active adapter (used by InstallTrace and
// tests).
func SetTraceAdapter(a *TraceAdapter) {
	adapterMu.Lock()
	currentTraceAdapter = a
	adapterMu.Unlock()
}

// CurrentTraceAdapter returns the active adapter, or nil.
func CurrentTraceAdapter() *TraceAdapter {
	adapterMu.RLock()
	defer adapterMu.RUnlock()
	return currentTraceAdapter
}

// RebindTrace switches the active adapter to a new session. Nil-safe
// (trading disabled / never installed → no-op). Called by cmd/metis
// rebindSession on /new, /resume and /branch so subsequent events land
// in the new session's trace file.
func RebindTrace(sessionID string) {
	if a := CurrentTraceAdapter(); a != nil {
		a.SetSession(sessionID)
	}
}

// CurrentTraceStore returns the process-wide trace store installed by
// InstallTrace, or nil when tracing is disabled. Observers (the web UI
// trajectory endpoint) read through it; it must never be Closed by the
// caller - it stays alive for the whole process.
func CurrentTraceStore() *session.TraceStore {
	adapter := CurrentTraceAdapter()
	if adapter == nil {
		return nil
	}
	return adapter.store
}
