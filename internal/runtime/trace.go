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
	"context"
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

	mu        sync.Mutex
	sessionID string
	// activeTurns is session-scoped rather than one process-global current
	// turn. Desktop may keep session A running while the user selects and starts
	// session B; a late terminal event from A must not close or renumber B.
	activeTurns map[string]int
	// invocationOrigin is keyed exclusively by a process-unique internal ID,
	// never a provider tool_use_id. legacyChildOrigin preserves compatibility
	// for embedders/tests that construct old Event values without the new ID,
	// but even that fallback uses session+turn+provider ID and refuses ambiguous
	// matches instead of globally assigning one provider ID to one owner.
	invocationOrigin  map[string]*traceInvocationState
	legacyChildOrigin map[legacyTraceKey]traceOrigin
	turnUsage         map[traceTurnKey]session.CostSnapshot
	resolvedObserver  func(ResolvedTraceEvent)

	// Streaming deltas are coalesced, but the buffer must carry its immutable
	// owner. A background child can outlive the parent turn or a session switch;
	// consulting sessionID only when the burst is flushed would write
	// that old child text into the new active session or merge it with parent text.
	burstKind         string
	burstSessionID    string
	burstTurn         int
	burstSubAgentOf   string
	burstInvocationID string
	burstBuf          strings.Builder
}

type traceOrigin struct {
	sessionID    string
	turn         int
	subAgent     string
	invocationID string
}

type traceInvocationState struct {
	origin         traceOrigin
	root           bool
	lifecycleDepth int
	resultSeen     bool
}

type traceTurnKey struct {
	sessionID string
	turn      int
}

type legacyTraceKey struct {
	sessionID string
	turn      int
	toolUseID string
}

// TraceEventOrigin is the immutable owner assigned by TraceAdapter. The
// InvocationID is process-local observability metadata; SessionID+Turn are the
// durable keys consumers should persist.
type TraceEventOrigin struct {
	SessionID    string
	Turn         int
	InvocationID string
}

// ResolvedTraceEvent is delivered after TraceAdapter releases its mutex. For
// EventTokens, CumulativeUsage is the absolute provider-usage sum for this
// immutable session+turn, including trace rows from before observer install or
// process restart. Consumers can therefore upsert idempotently rather than
// incrementing from whichever Desktop session happens to be active.
type ResolvedTraceEvent struct {
	Event           agent.Event
	SessionID       string
	Turn            int
	InvocationID    string
	CumulativeUsage session.CostSnapshot
}

const redactedThinkingPlaceholder = "Reasoning redacted by provider"

// NewTraceAdapter wraps a TraceStore.
func NewTraceAdapter(store *session.TraceStore) *TraceAdapter {
	return &TraceAdapter{
		store:             store,
		invocationOrigin:  make(map[string]*traceInvocationState),
		legacyChildOrigin: make(map[legacyTraceKey]traceOrigin),
		activeTurns:       make(map[string]int),
		turnUsage:         make(map[traceTurnKey]session.CostSnapshot),
	}
}

// SetSession switches the active trace session. Called on every
// /new /resume /branch and web-session switch, so each channel of
// events lands in its own JSONL file.
func (a *TraceAdapter) SetSession(sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	burstSessionID := a.burstSessionID
	a.flushBurstLocked()
	if a.store != nil && a.sessionID != "" {
		a.syncSessionLocked(a.sessionID)
	}
	if burstSessionID != "" && burstSessionID != a.sessionID {
		a.syncSessionLocked(burstSessionID)
	}
	a.sessionID = sid
}

// SetResolvedEventObserver installs the adapter's single resolved-event
// observer. Runtime assembly (for example the Desktop server) should install
// it once, not per request. Passing nil clears it. The observer is invoked
// synchronously after the adapter mutex has been released, so it may safely
// persist or query trace state without re-entering OnEvent's critical section.
func (a *TraceAdapter) SetResolvedEventObserver(fn func(ResolvedTraceEvent)) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.resolvedObserver = fn
	a.mu.Unlock()
}

// BindTraceTurn returns a context whose root-loop events stay pinned to
// sessionID and the adapter's current turn even if another Desktop session is
// selected before the provider emits its final usage. Call after
// RecordUserMessage so both the USER anchor and MessageMetric share the exact
// turn returned here.
func BindTraceTurn(ctx context.Context, sessionID string) (context.Context, TraceEventOrigin) {
	if ctx == nil {
		ctx = context.Background()
	}
	adapter := CurrentTraceAdapter()
	if adapter == nil || adapter.store == nil || sessionID == "" {
		return ctx, TraceEventOrigin{}
	}
	id := agent.NewTraceInvocationID()
	adapter.mu.Lock()
	turn := adapter.ensureTurnLocked(sessionID)
	origin := traceOrigin{sessionID: sessionID, turn: turn, invocationID: id}
	// Bind itself owns one lifecycle level. EndTraceTurn releases it only
	// after the root Loop.Run has fully unwound.
	adapter.invocationOrigin[id] = &traceInvocationState{origin: origin, root: true, lifecycleDepth: 1}
	adapter.mu.Unlock()
	return agent.WithTraceInvocationID(ctx, id), TraceEventOrigin{
		SessionID: sessionID, Turn: turn, InvocationID: id,
	}
}

// EndTraceTurn releases the immutable root origin created by BindTraceTurn.
// Call it after Loop.Run returns so deferred events emitted during Run cleanup
// (for example a resend notice after provider failure) remain attached to the
// terminal turn instead of being dropped or reassigned to the selected session.
func EndTraceTurn(ctx context.Context) {
	id := agent.TraceInvocationIDFromContext(ctx)
	if id == "" {
		return
	}
	adapter := CurrentTraceAdapter()
	if adapter == nil {
		return
	}
	// Root-turn binding is runtime-owned, so release it directly instead of
	// depending on whichever process-global agent hook an embedder installed.
	// Child tool lifecycles still flow through agent.TraceInvocationEnded.
	adapter.OnEvent(agent.Event{Kind: agent.EventTraceInvocationEnd, TraceInvocationID: id})
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
	origin := traceOrigin{sessionID: sessionID, turn: a.ensureTurnLocked(sessionID)}
	a.appendLocked(origin, "user", agent.Event{Kind: agent.EventChannelInbound}, TraceExtra{Text: strings.TrimSpace(text)})
}

func (a *TraceAdapter) OnEvent(ev agent.Event) {
	var resolved *ResolvedTraceEvent
	a.mu.Lock()
	defer func() {
		observer := a.resolvedObserver
		a.mu.Unlock()
		if observer != nil && resolved != nil {
			observer(*resolved)
		}
	}()

	if a.store == nil {
		return
	}
	if ev.Kind == agent.EventTraceInvocationStart || ev.Kind == agent.EventTraceInvocationEnd {
		// Lifecycle boundaries separate Ralph rounds even when the provider
		// emits adjacent text deltas with the same outer invocation ID.
		a.flushBurstLocked()
		a.noteInvocationLifecycleLocked(ev)
		return
	}
	if ev.Kind == agent.EventStreamStart || ev.Kind == agent.EventStreamEnd {
		return // transport noise — not part of the agent's trajectory
	}
	origin, ok := a.originForEventLocked(ev)
	if !ok {
		// A child without a known parent origin must never fall back to the
		// currently selected session. In normal execution the top-level Agent
		// tool_start is traced synchronously before the child can emit anything;
		// a miss therefore means the event is stale or structurally incomplete.
		return
	}
	resolved = &ResolvedTraceEvent{
		Event: ev, SessionID: origin.sessionID, Turn: origin.turn,
		InvocationID: origin.invocationID,
	}
	if ev.Kind == agent.EventTokens {
		resolved.CumulativeUsage = a.addTurnUsageLocked(origin, ev)
	}

	// Adjacent streaming deltas are aggregated into typed bursts. A burst
	// ends when its type OR immutable owner changes, or at the first non-delta
	// event, preserving provider order without combining parent/child streams.
	switch ev.Kind {
	case agent.EventTextDelta:
		a.bufferDeltaLocked(origin, "text", ev.TextDelta)
		return
	case agent.EventToolStart:
		a.flushBurstLocked()
		inputJSON, _ := json.Marshal(ev.ToolInput)
		parent := ev.SubAgentParentID
		a.appendLocked(origin, "tool_start", ev, TraceExtra{
			ToolName:  ev.ToolName,
			ToolUseID: ev.ToolUseID,
			Text:      string(inputJSON),
			ParentID:  parent,
		})
		if isChildAgentTool(ev.ToolName) && ev.ToolUseID != "" {
			childOrigin := traceOrigin{
				sessionID:    origin.sessionID,
				turn:         origin.turn,
				subAgent:     ev.ToolUseID,
				invocationID: ev.TraceInvocationID,
			}
			if ev.TraceInvocationID != "" {
				a.invocationOrigin[ev.TraceInvocationID] = &traceInvocationState{origin: childOrigin}
			} else {
				// Compatibility only. Real dispatch always supplies the internal
				// invocation ID. Include the immutable owner in the fallback key so
				// duplicate provider IDs cannot overwrite one another.
				a.legacyChildOrigin[legacyTraceKey{
					sessionID: origin.sessionID,
					turn:      origin.turn,
					toolUseID: ev.ToolUseID,
				}] = childOrigin
			}
		}
	case agent.EventToolResult:
		a.flushBurstLocked()
		var out string
		var isErr bool
		if ev.ToolResult != nil {
			out = ev.ToolResult.Output
			isErr = ev.ToolResult.IsError
		}
		a.appendLocked(origin, "tool_result", ev, TraceExtra{
			ToolName:  ev.ToolName,
			ToolUseID: ev.ToolUseID,
			Text:      out,
			IsError:   isErr,
			ElapsedMs: ev.Elapsed.Milliseconds(),
			ParentID:  ev.ToolUseID,
		})
		if isChildAgentTool(ev.ToolName) {
			a.noteInvocationResultLocked(ev.TraceInvocationID)
			if ev.TraceInvocationID == "" {
				a.deleteLegacyOriginLocked(origin, ev.ToolUseID)
			}
		}
	case agent.EventTurnEnd:
		a.flushBurstLocked()
		a.appendLocked(origin, "turn_end", ev, TraceExtra{})
		if ev.SubAgentParentID == "" {
			a.syncSessionLocked(origin.sessionID)
		}
	case agent.EventLoopDone:
		a.flushBurstLocked()
		a.appendLocked(origin, "loop_done", ev, TraceExtra{Text: ev.StopReason})
		a.syncSessionLocked(origin.sessionID)
		if ev.SubAgentParentID == "" {
			delete(a.activeTurns, origin.sessionID)
		} else if ev.TraceInvocationID == "" {
			a.deleteLegacyOriginLocked(origin, ev.SubAgentParentID)
		}
	case agent.EventTokens:
		a.flushBurstLocked()
		a.appendLocked(origin, "tokens", ev, TraceExtra{Text: fmt.Sprintf(
			"input=%d output=%d cache_write=%d cache_read=%d",
			ev.InputTokens, ev.OutputTokens, ev.CacheCreationInputTokens, ev.CacheReadInputTokens)})
		// Make the recovery source durable before the resolved observer commits
		// cost.json. The ledger can also repair a missing metric by itself, but
		// this ordering preserves the complete trajectory across a hard crash.
		a.syncSessionLocked(origin.sessionID)
	case agent.EventError:
		a.flushBurstLocked()
		a.appendLocked(origin, "error", ev, TraceExtra{
			Text: fmt.Sprint(ev.Err), IsError: true,
		})
		// Every EventError emitted by Loop.Run is immediately followed by a
		// return, not LoopDone. Treat it as the durable terminal boundary: a
		// top-level error closes the user turn, while a child error only closes
		// that child origin and must not disturb its parent turn.
		a.syncSessionLocked(origin.sessionID)
		if ev.SubAgentParentID == "" {
			delete(a.activeTurns, origin.sessionID)
		} else if ev.TraceInvocationID == "" {
			a.deleteLegacyOriginLocked(origin, ev.SubAgentParentID)
		}
	case agent.EventPermissionRequest:
		a.flushBurstLocked()
		a.appendLocked(origin, "permission_request", ev, TraceExtra{
			ToolName: ev.PermissionTool,
			Text:     ev.PermissionReason,
		})
	case agent.EventSubAgentStart, agent.EventSubAgentEnd, agent.EventSubAgentProgress:
		a.flushBurstLocked()
		k := eventKindLabel(ev.Kind)
		a.appendLocked(origin, k, ev, TraceExtra{
			ToolUseID: ev.ToolUseID,
			ParentID:  ev.SubAgentParentID,
			Text:      ev.Info,
		})
	case agent.EventPlan:
		a.flushBurstLocked()
		a.appendLocked(origin, "plan", ev, TraceExtra{Text: ev.Info})
	case agent.EventRateLimitHit:
		a.flushBurstLocked()
		a.appendLocked(origin, "rate_limit", ev, TraceExtra{Text: ev.Info})
	case agent.EventModelFallback:
		a.flushBurstLocked()
		a.appendLocked(origin, "model_fallback", ev, TraceExtra{Text: ev.Info})
	case agent.EventContextWarn, agent.EventContextCompacted, agent.EventCompactionStart, agent.EventCompactionProgress, agent.EventCompactionEnd:
		a.flushBurstLocked()
		a.appendLocked(origin, eventKindLabel(ev.Kind), ev, TraceExtra{Text: ev.Info})
	case agent.EventThinkingDelta:
		a.bufferDeltaLocked(origin, "thinking", ev.TextDelta)
		return
	case agent.EventRedactedThinking:
		a.flushBurstLocked()
		// The event carries opaque provider ciphertext in TextDelta solely
		// for transcript round-tripping. Trace/UI surfaces must never store it.
		a.appendLocked(origin, "thinking_redacted", ev, TraceExtra{Text: redactedThinkingPlaceholder})
		return
	case agent.EventToolArgsDelta:
		a.flushBurstLocked()
		a.appendLocked(origin, "tool_args", ev, TraceExtra{
			ToolName:  ev.ToolName,
			ToolUseID: ev.ToolUseID,
			Text:      ev.TextDelta,
		})
	case agent.EventAskUser:
		a.flushBurstLocked()
		a.appendLocked(origin, "ask_user", ev, TraceExtra{Text: ev.AskUserQuestion})
	case agent.EventChannelInbound, agent.EventChannelSent, agent.EventHookFired,
		agent.EventDreamingStart, agent.EventDreamingProgress, agent.EventDreamingEnd:
		a.flushBurstLocked()
		a.appendLocked(origin, eventKindLabel(ev.Kind), ev, TraceExtra{Text: ev.Info})
	default:
		// EventInfo and anything else lands as an "info" event.
		a.flushBurstLocked()
		a.appendLocked(origin, "info", ev, TraceExtra{Text: ev.Info})
	}
}

func (a *TraceAdapter) originForEventLocked(ev agent.Event) (traceOrigin, bool) {
	if ev.TraceInvocationID != "" {
		if state := a.invocationOrigin[ev.TraceInvocationID]; state != nil {
			return state.origin, true
		}
		if ev.TraceParentInvocationID != "" {
			if parent := a.invocationOrigin[ev.TraceParentInvocationID]; parent != nil {
				origin := parent.origin
				origin.invocationID = ev.TraceInvocationID
				return origin, true
			}
			return traceOrigin{}, false
		}
		// The only legitimate unknown internal ID is a top-level child-tool
		// ToolStart: this event creates its mapping. All child/root events must
		// already have been bound, otherwise falling back to the currently
		// selected session would recreate the cross-session corruption this ID
		// exists to prevent.
		if ev.Kind != agent.EventToolStart || !isChildAgentTool(ev.ToolName) || ev.SubAgentParentID != "" {
			return traceOrigin{}, false
		}
		if a.sessionID == "" {
			return traceOrigin{}, false
		}
		return traceOrigin{
			sessionID:    a.sessionID,
			turn:         a.ensureTurnLocked(a.sessionID),
			invocationID: ev.TraceInvocationID,
		}, true
	}
	if ev.SubAgentParentID != "" {
		return a.legacyOriginForParentLocked(ev.SubAgentParentID)
	}
	if a.sessionID == "" {
		return traceOrigin{}, false
	}
	return traceOrigin{sessionID: a.sessionID, turn: a.ensureTurnLocked(a.sessionID)}, true
}

func (a *TraceAdapter) legacyOriginForParentLocked(parentID string) (traceOrigin, bool) {
	if parentID == "" {
		return traceOrigin{}, false
	}
	// Prefer the selected session only when exactly one live fallback owner
	// exists there. If IDs were reused, dropping an unresolvable legacy event
	// is safer than silently corrupting either turn.
	var selected traceOrigin
	selectedCount := 0
	var sole traceOrigin
	total := 0
	for key, origin := range a.legacyChildOrigin {
		if key.toolUseID != parentID {
			continue
		}
		total++
		sole = origin
		if key.sessionID == a.sessionID {
			selected = origin
			selectedCount++
		}
	}
	if selectedCount == 1 {
		return selected, true
	}
	if selectedCount > 1 || total != 1 {
		return traceOrigin{}, false
	}
	return sole, true
}

func (a *TraceAdapter) deleteLegacyOriginLocked(origin traceOrigin, toolUseID string) {
	delete(a.legacyChildOrigin, legacyTraceKey{
		sessionID: origin.sessionID,
		turn:      origin.turn,
		toolUseID: toolUseID,
	})
}

func isChildAgentTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "agent", "fork", "ralph":
		return true
	default:
		return false
	}
}

func (a *TraceAdapter) noteInvocationLifecycleLocked(ev agent.Event) {
	if ev.TraceInvocationID == "" {
		return
	}
	state := a.invocationOrigin[ev.TraceInvocationID]
	if state == nil {
		return
	}
	if ev.Kind == agent.EventTraceInvocationStart {
		state.lifecycleDepth++
		return
	}
	if state.lifecycleDepth > 0 {
		state.lifecycleDepth--
	}
	if state.root {
		if state.lifecycleDepth == 0 {
			a.syncSessionLocked(state.origin.sessionID)
			delete(a.activeTurns, state.origin.sessionID)
			delete(a.invocationOrigin, ev.TraceInvocationID)
		}
		return
	}
	if state.lifecycleDepth == 0 && state.resultSeen {
		delete(a.invocationOrigin, ev.TraceInvocationID)
	}
}

func (a *TraceAdapter) noteInvocationResultLocked(invocationID string) {
	if invocationID == "" {
		return
	}
	state := a.invocationOrigin[invocationID]
	if state == nil {
		return
	}
	state.resultSeen = true
	if state.lifecycleDepth == 0 {
		delete(a.invocationOrigin, invocationID)
	}
}

func (a *TraceAdapter) addTurnUsageLocked(origin traceOrigin, ev agent.Event) session.CostSnapshot {
	key := traceTurnKey{sessionID: origin.sessionID, turn: origin.turn}
	usage, ok := a.turnUsage[key]
	if !ok {
		usage = a.persistedTurnUsageLocked(key)
	}
	usage.InputTokens += ev.InputTokens
	usage.OutputTokens += ev.OutputTokens
	usage.CacheCreateTokens += ev.CacheCreationInputTokens
	usage.CacheReadTokens += ev.CacheReadInputTokens
	a.turnUsage[key] = usage
	return usage
}

func (a *TraceAdapter) persistedTurnUsageLocked(key traceTurnKey) session.CostSnapshot {
	var usage session.CostSnapshot
	if a.store == nil || key.sessionID == "" || key.turn <= 0 {
		return usage
	}
	for _, ev := range a.store.Events(key.sessionID) {
		if ev.Turn != key.turn || ev.Kind != "tokens" {
			continue
		}
		var in, out, cacheWrite, cacheRead int
		if _, err := fmt.Sscanf(ev.Text, "input=%d output=%d cache_write=%d cache_read=%d", &in, &out, &cacheWrite, &cacheRead); err != nil {
			continue
		}
		usage.InputTokens += in
		usage.OutputTokens += out
		usage.CacheCreateTokens += cacheWrite
		usage.CacheReadTokens += cacheRead
	}
	return usage
}

func (a *TraceAdapter) syncSessionLocked(sessionID string) {
	if a.store == nil {
		return
	}
	if err := a.store.SyncSession(sessionID); err != nil {
		log.Printf("trace: flush session %s: %v", sessionID, err)
	}
}

// Flush commits any in-flight streaming burst and all buffered trace writes.
// It is used by process cleanup, including a Desktop backend interrupted while
// a response is still streaming and therefore has not emitted LoopDone yet.
func (a *TraceAdapter) Flush() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		return nil
	}
	a.flushBurstLocked()
	return a.store.Sync()
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

func (a *TraceAdapter) appendLocked(origin traceOrigin, kind string, ev agent.Event, x TraceExtra) {
	te := &session.TraceEvent{
		SessionID:               origin.sessionID,
		Turn:                    origin.turn,
		Kind:                    kind,
		ToolName:                x.ToolName,
		ToolUseID:               x.ToolUseID,
		Text:                    truncateTrace(x.Text, 2000),
		IsError:                 x.IsError,
		ElapsedMs:               x.ElapsedMs,
		ParentID:                x.ParentID,
		TraceInvocationID:       origin.invocationID,
		TraceParentInvocationID: ev.TraceParentInvocationID,
		TraceCallID:             ev.TraceCallID,
	}
	if origin.subAgent != "" && x.ParentID == "" {
		te.SubAgentOf = origin.subAgent
	}
	if err := a.store.Append(te); err != nil {
		log.Printf("trace: append event: %v", err)
	}
}

// ensureTurnLocked opens a new turn if one isn't active.
func (a *TraceAdapter) ensureTurnLocked(sid string) int {
	if turn := a.activeTurns[sid]; turn > 0 {
		return turn
	}
	turn := a.store.NextTurn(sid)
	a.activeTurns[sid] = turn
	return turn
}

// bufferDeltaLocked appends a provider delta to the active typed burst,
// settling the previous burst first when the provider changes type.
func (a *TraceAdapter) bufferDeltaLocked(origin traceOrigin, kind, delta string) {
	if delta == "" {
		return
	}
	if a.burstKind != "" && (a.burstKind != kind ||
		a.burstSessionID != origin.sessionID ||
		a.burstTurn != origin.turn ||
		a.burstSubAgentOf != origin.subAgent ||
		a.burstInvocationID != origin.invocationID) {
		a.flushBurstLocked()
	}
	a.burstKind = kind
	a.burstSessionID = origin.sessionID
	a.burstTurn = origin.turn
	a.burstSubAgentOf = origin.subAgent
	a.burstInvocationID = origin.invocationID
	a.burstBuf.WriteString(delta)
}

// flushBurstLocked emits the accumulated typed delta burst as one event.
func (a *TraceAdapter) flushBurstLocked() {
	if a.burstBuf.Len() == 0 {
		a.clearBurstLocked()
		return
	}
	kind := a.burstKind
	sid := a.burstSessionID
	turn := a.burstTurn
	subAgentOf := a.burstSubAgentOf
	invocationID := a.burstInvocationID
	text := strings.TrimSpace(a.burstBuf.String())
	a.clearBurstLocked()
	if text == "" || sid == "" || turn <= 0 {
		return
	}
	if err := a.store.Append(&session.TraceEvent{
		SessionID:         sid,
		Turn:              turn,
		Kind:              kind,
		Text:              truncateTrace(text, 2000),
		SubAgentOf:        subAgentOf,
		TraceInvocationID: invocationID,
	}); err != nil {
		log.Printf("trace: append %s event: %v", kind, err)
	}
}

func (a *TraceAdapter) clearBurstLocked() {
	a.burstBuf.Reset()
	a.burstKind = ""
	a.burstSessionID = ""
	a.burstTurn = 0
	a.burstSubAgentOf = ""
	a.burstInvocationID = ""
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
	if adapter == nil {
		return
	}
	if err := adapter.Flush(); err != nil {
		log.Printf("trace: flush on shutdown: %v", err)
	}
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
