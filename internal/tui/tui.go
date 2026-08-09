package tui

// tui.go — Model definition + lifecycle. The bubbletea Update/View
// loop and the per-feature handlers live in sibling files:
//   tui_update.go    — Update() + finalizeTurn + runTurnAsync + persistTail
//   tui_events.go    — handleAgentEvent (drains agent.Event into Model state)
//   tui_render.go    — View() + timeline plumbing
//   tui_styles.go    — color palette + initStyles()
//   render_*.go      — per-section rendering (welcome / message / tool / overlay / chrome)
//   keybind_*.go     — per-section key handling (main / palette / permission / submit / session)
//   tui_spinner.go   — spinner frames + tickCmd
//
// Keep tui.go focused on Model state + NewModel + RunTUI so a reader
// looking for "what's in the chat surface" finds it in one place.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tui/list"
	"github.com/Ricardo-M-L/metis/internal/tui/overlay"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// ============================================================================
// Public types
// ============================================================================

type Message struct {
	// ID is process-stable for the lifetime of a TUI session. Filled in
	// by m.nextID() at the point we append to m.messages, used for
	// future cross-feature references (multi-pane navigation, click-to-
	// expand, etc.). NOT a cache key — renderCache keys on (role,
	// content, width) so the (×N) error-dedupe content rewrite path
	// invalidates correctly without ID help.
	ID        string
	Role      string
	Content   string
	ToolName  string
	ToolError bool
	Timestamp time.Time
}

// subAgentLingerDuration is how long a finished sub-agent's pill
// stays on screen after its Status flips to completed/failed. Long
// enough for a glance ("did it succeed?") but short enough that a
// burst of N quick spawns doesn't pile up dead pills in the chip
// row. 2s mirrors claude-code's Task pill tail.
const subAgentLingerDuration = 2 * time.Second

// SubAgentInfo is one in-flight (or recently finished) sub-agent.
// Used for status-bar pill visualization; rendered as
// "◇ Name · LastTool · 23s · 7t" while running, "✓ Name · 47s · 12t"
// for ~2s after completion, "✗ Name" for ~2s after failure.
// Populated by handleAgentEvent when an Agent tool call fires;
// ToolsCount and LastTool are bumped by forwarded sub-agent
// EventToolStart events (matched on SubAgentParentID); Status flips
// + FinishedAt stamps on the EventToolResult for the Agent itself.
type SubAgentInfo struct {
	ID        string
	Name      string
	Status    string // running | completed | failed
	StartedAt time.Time
	// FinishedAt is non-zero once the sub-agent's tool_result has
	// arrived. The TUI's spinner-tick pruner removes the pill ~2s
	// after this, so users see a brief ✓/✗ tail before it vanishes.
	FinishedAt time.Time
	// ToolsCount is the number of EventToolStart events forwarded
	// from this sub-agent's own loop. Mirrors claude-code's Task
	// pill which shows "N tools" mid-flight so the user can tell a
	// stuck sub-agent from a busy one.
	ToolsCount int
	// LastTool is the name of the most recent tool the sub-agent
	// started (without the "sub: " prefix). Empty until the first
	// child tool fires.
	LastTool string
}

type ToolEvent struct {
	// ID is process-stable; filled at EventToolStart and preserved
	// across the start→result mutation. Same role as Message.ID:
	// future cross-feature linking, not a cache key.
	ID        string
	Kind      string
	ToolName  string
	Input     map[string]any
	Output    string
	IsError   bool
	Duration  time.Duration
	StartTime time.Time
	// SubAgentParentID is set (to the parent Agent tool's tool_use_id)
	// when this tool call was forwarded from a child sub-agent loop.
	// Non-empty → the row renders INDENTED under its parent agent row
	// instead of flat at top level, so the tree reads
	// "agent(x) → glob → grep" (claude-code's nested display) rather
	// than a flat "agent(x)" followed by a top-level "sub: glob".
	SubAgentParentID string
}

type toolArgsStreamState struct {
	data     []byte
	toolName string
	seq      uint64
}

type permChoice struct {
	Label string
	Key   string
}

// ============================================================================
// Model
// ============================================================================

// ExternalHooks lets the cmd/metis layer hand the TUI a few callbacks
// for features whose state lives in the runtime layer (dirs, sub-agent
// query, etc.) without forcing internal/tui to import internal/runtime
// (which would cycle through cfg/llm/etc).
//
// Optional — nil callbacks degrade to a friendly "feature unavailable"
// message rather than panicking.
type ExternalHooks struct {
	DirAdd    func(path string, persist bool) error
	DirRemove func(path string) error
	DirList   func() []string
	// BtwAsk fires a single-turn LLM call with no tools and no history
	// write. Returns the assistant text, or an error. Implementation
	// expected to share the parent's prompt cache.
	BtwAsk func(ctx context.Context, question string) (string, error)
	// SessionSwitch rebinds process/runtime-owned session routers (Todo/Task
	// stores, prompt dumps and working-tree checkpoints). The TUI owns the
	// transcript, gate, timing, title and cost portions of the same boundary.
	SessionSwitch func(sessionID string)
	// SessionBoundary releases process-owned work tied to the session being
	// left (for example background bash jobs and monitor goroutines). It runs
	// only after destination preflight succeeds and before the new ID commits.
	SessionBoundary func()
	// FreshPermissionMode is the invocation-resolved mode after CLI/profile
	// overrides. A fresh/forked session must use this baseline, not inherit a
	// resumed session's bypass/deny posture.
	FreshPermissionMode permission.Mode
}

type Model struct {
	ctx  context.Context
	loop *agent.Loop
	// cronSvc backs the in-session scheduler (cron_scheduler.go) — the same
	// CronService the CronCreate/List/Delete tools mutate, so session-only
	// jobs the model schedules mid-chat fire here. nil ⇒ no in-session ticks.
	cronSvc   *agent.CronService
	gate      *permission.Gate
	slash     *slash.Registry
	session   *session.Store
	sessionID string
	// sessionTitle is the human-friendly label set via /rename or /title
	// (persisted in the session header by session.Store.SetTitle).
	// Used to drive the terminal window title (bubbletea's
	// tea.View.WindowTitle) and the exit-time resume hint. Initialized
	// from the session header at NewModel time so a resumed session's
	// previously-set title takes effect on first frame; updated in
	// keybind_submit.go's SignalTitle handler when the user renames.
	sessionTitle string
	// baseSystem is the invocation-level system prompt. Old session files may
	// omit System; falling back here prevents the system prompt of the session
	// being left from leaking into a fresh/legacy destination.
	baseSystem         string
	baseSystemSections []llm.SystemSection
	model              string
	// providerName tracks the provider profile the running Loop.Provider
	// was built from (cfg.Provider.Default at startup, or whichever
	// profile the user picked via /model). Required for mid-session
	// model switches to know which profile to rebuild against —
	// switchModel calls rtpkg.BuildProvider(m.cfg, providerName, model)
	// when the user changes models within the same profile.
	providerName string
	skillDir     string
	cmds         *REPLCommandRegistry
	ext          ExternalHooks
	// cfg is the loaded ~/.metis/config.toml. The TUI reaches into a
	// few sub-sections directly (Tools.Bash for !bash-mode shell
	// settings, Channels for SendMessage routing, etc.). Storing the
	// whole Config keeps Model self-sufficient — feature additions
	// don't need to thread new params through NewModel each time.
	cfg *config.Config

	messages   []Message
	toolEvents []ToolEvent
	// recoveryPlanCache keeps the v0.4.12 recovered-error grouping pass off
	// the 40ms spinner hot path. The plan only changes when the historical
	// message/tool slices change; elapsed time, spinner glyphs, token counts,
	// and streaming-tail text do not affect it. Without this cache every frame
	// rescanned every historical error output (including large Agent/Bash
	// payloads), which made the elapsed clock visibly skip under busy
	// multi-agent sessions.
	recoveryPlanCache      map[*ToolEvent]*recoveredErrorGroupPlan
	recoveryPlanCacheKey   recoveryPlanCacheKey
	recoveryPlanCacheValid bool
	// turnToolEventStart is the first toolEvents index owned by the current
	// top-level UI turn. Historical tool rows stay mounted for the lifetime of
	// the session, but recap/learning summaries must only inspect this suffix.
	turnToolEventStart int
	// historyCursor tracks the durable prefix of loop.History(). Unlike the
	// old "walk back to the last user text" heuristic it preserves assistant
	// tool_use + user tool_result messages that precede a mid-turn steer.
	historyCursor session.HistoryCursor
	// thinkingText accumulates extended-thinking deltas for the
	// in-flight turn. Rendered live above the streaming reply with
	// dim/italic styling; flushed into a "thinking" Message when the
	// next text or tool block starts (so the trace persists in the
	// transcript instead of vanishing at turn end).
	thinkingText string
	// imagePaste indexes pasted clipboard images by display tag so
	// the input can show `[Image #1]` (claude-code style) while the
	// submit pipeline still resolves to the cached file path.
	// Reset after submit; never persisted across turns.
	imagePaste   map[int]string // N -> /path/to/cached.png
	imageCounter int            // 1-based; matches the visible #N

	// input is the chat-surface multi-line editor. textarea (instead of
	// textinput) lets the user paste multi-line code and split prompts
	// across rows with Alt+Enter / Ctrl+J. Enter still submits — handleKey
	// intercepts KeyEnter before it reaches textarea.
	input textarea.Model
	// chatList is a virtualized list (internal/tui/list) that renders
	// only the items intersecting the current viewport. Replaces the
	// previous bubbles/viewport.Model, which paid O(N) string-cat per
	// frame for the full transcript — see metis-tranquil-lemon.md
	// "C方案" rationale: 1200-item realistic-session benchmarks went
	// from 5.6 MB allocs/frame (viewport) to ~150 KB (list).
	chatList      *list.List
	turnActive    bool
	streamingText string

	spinnerActive    bool
	spinnerFrame     int
	spinnerStartedAt time.Time
	spinnerVerb      string
	spinnerSub       string
	// spinnerOverride pins the spinner verb to a fixed label that wins
	// over spinnerVerb / spinnerSub while non-empty. Used when the loop
	// is in an LLM-driven compaction phase (Collapse / Compact) so the
	// user sees "Compacting conversation..." instead of the thinking
	// verb that would otherwise show during a 5-30s summarize call —
	// that was the "input area looks frozen" user report (2026-05-15
	// screenshot #3). Cleared on EventContextCompacted.
	spinnerOverride string

	// spinnerCompactionBytes is the cumulative byte count of the
	// in-flight summarize stream, updated by EventCompactionProgress.
	// It is an activity signal only: the final summary size is unknown,
	// so it cannot be converted into a truthful completion percentage.
	// Reset on EventContextCompacted.
	spinnerCompactionBytes int

	// compactionStartedAt is the timestamp of the most recent
	// EventCompactionStart. The monotonic progress estimate uses this
	// instead of spinnerStartedAt so it never restarts at a turn boundary.
	compactionStartedAt time.Time
	// spinnerPhase mirrors claude-code's SpinnerMode (sourcemap
	// restored-src/src/components/Spinner/SpinnerAnimationRow.tsx
	// lines 235-265). Drives the directional arrow:
	//   "requesting" → ↑   (uploading prompt / waiting for first byte)
	//   "thinking"   → ↓   (extended-thinking deltas streaming back)
	//   "responding" → ↓   (text deltas streaming back)
	//   "tool"       → ↓   (tool call in flight)
	// State transitions happen in handleAgentEvent; the renderer in
	// render_chrome.go switches arrow + count source on this field.
	spinnerPhase string
	// firstStreamAt records when the first text-delta of the current
	// turn arrived. (firstStreamAt - spinnerStartedAt) is the wall-time
	// the model spent before producing visible output, surfaced as
	// "thought for Xs" mimicking claude-code's thinkingStatus.
	firstStreamAt time.Time

	showPalette bool
	palFilter   string
	palCursor   int
	palMatched  []REPLCommand

	// Ctrl+R history search overlay state. Distinct from the slash
	// palette (`showPalette`) so a user can hit `/` mid-history-search
	// without us having to multiplex two filter strings into one.
	// Loaded lazily on first open from ~/.metis/history.jsonl.
	// transcript search (F10, Ctrl+F): full-text find within current
	// session's messages. Distinct from showHistory which searches
	// past prompts across all sessions.
	showSearch  bool
	searchQuery string
	searchHits  []int // message indices matching searchQuery
	searchCur   int   // current index into searchHits
	showHistory bool
	histAll     []string // newest-first dedup'd input strings
	histFilter  string
	histCursor  int
	histMatched []string // subset of histAll that fuzzy-matches histFilter

	// Direct ↑/↓ history navigation (T7) — separate from the Ctrl+R
	// overlay above. When the input is empty or its content was last
	// loaded from history, ↑ walks back through histAll one entry at
	// a time and ↓ walks forward. histDirectIdx == -1 means "not
	// navigating yet"; ≥ 0 indexes into histAll. histDirectDraft is
	// the user's in-progress text saved when nav started, restored
	// when ↓ walks past index 0.
	histDirectIdx   int
	histDirectDraft string

	// Streaming tool args buffers (T12), keyed by ToolUseID. Providers may
	// interleave deltas for several parallel calls; a single global byte slice
	// mixed their JSON and one result incorrectly erased every preview. The
	// empty key retains compatibility with legacy producers that omit IDs.
	toolArgsStreams map[string]toolArgsStreamState
	toolArgsSeq     uint64

	// @-mention dropdown state. Tracked separately from the slash
	// palette so an in-progress `@xxx` filter doesn't fight the slash
	// palette's `palFilter` for the same buffer. Recomputed on every
	// key by `updateAtMention()` (called from the textarea-update
	// codepath), so we don't have to drive it from a dedicated key
	// handler — it just appears when the cursor is in an `@xxx` token.
	atActive  bool
	atFilter  string
	atCursor  int      // selected row in atMatched (0-based)
	atMatched []string // current fuzzy-matched file paths

	permActive   bool
	permQuestion string
	permTool     string // tool name being asked about (Bash/Edit/etc)
	permArgs     string // truncated arg preview (command / path / etc)
	permChoices  []permChoice
	permCursor   int
	// permReply is the reply channel handed to us via the agent loop's
	// EventPermissionRequest. We send exactly one decision through it
	// to unblock the tool dispatcher.
	permReply chan agent.PermissionDecision
	// permStartedAt is the wall clock when the prompt appeared. The
	// spinner tick uses it to drive the visible countdown and to
	// auto-deny once permissionTimeout elapses — protects against the
	// "user walked away from VNC, agent stuck for hours on a Yes/No"
	// failure mode the user hit during cross-CLI testing.
	permStartedAt time.Time

	// AskUser prompt state — set when the model dispatches the AskUser
	// tool. While askUserActive is true, the prompt blocks the
	// keyboard for selection (1-9 / arrows / Enter / Esc / Tab to
	// freeform). askUserReply is the buffered (size-1) channel the
	// tool's Execute is blocked on; sending the chosen string unblocks
	// it. Mirrors permission-prompt state shape so the renderers /
	// handlers stay parallel.
	askUserActive   bool
	askUserQuestion string
	askUserOptions  []string
	// askUserAllowFreeform: when true, an extra "type your own answer"
	// row appears below the numbered options. Tab moves focus into the
	// text input; Enter submits the typed answer. When false, only
	// numbered selection is possible.
	askUserAllowFreeform bool
	askUserCursor        int // selected option index (0-based)
	// askUserFreeformOn — focus is on the freeform input rather than
	// the option list. Tab toggles this when allowFreeform is true.
	askUserFreeformOn bool
	askUserInput      textinput.Model
	askUserReply      chan string
	askUserStartedAt  time.Time

	width, height int
	startTime     time.Time
	totalTokens   tokenTracker
	showBanner    bool
	firstRender   bool
	// subAgents lists active sub-agent invocations (Agent tool calls)
	// for visualization as pills in the status bar.
	subAgents []SubAgentInfo
	// copyMode is on when the user pressed Ctrl+S to leave the
	// alt-screen so they can mouse-select-and-copy chat content.
	// While true, View() returns empty so the terminal stays still
	// for selection.
	copyMode bool
	// showTaskPanel toggles a Ctrl+T overlay listing the session's
	// todos with status glyphs.
	showTaskPanel bool
	// shellMode flips the next input from "agent prompt" to "literal
	// bash command". Toggled by Ctrl+X (Phase F #62). The submit
	// handler reads this flag to decide which pipeline to send to.
	// Until the submit-side dispatch lands, /shell shows a hint row
	// so the user can opt in via slash if the keybind isn't bound in
	// their setup.
	shellMode bool
	// queuedPrompts holds plain-text prompts the user typed while a
	// turn was already in flight. 2026-05-21 — upgraded from `[]string`
	// FIFO to `[]queuedItem` carrying Priority + arrival-time. Three
	// priorities (Now > Next > Later) mirror claude-code's
	// messageQueueManager; current UI only emits Priority=Next, but
	// the field is in place so a future `/now <text>` slash command
	// or background notification subsystem can opt into Now/Later
	// without a schema migration.
	//
	// Drain semantics: finalizeTurn pulls the highest-priority head
	// and BATCHES all adjacent items of the same priority into a
	// single user turn (joined with blank lines). This collapses
	// "follow-up follow-up follow-up" typing patterns into one LLM
	// round-trip instead of N — direct port of claude-code's
	// `dequeueAllMatching(targetMode)` batching.
	//
	// Ctrl+C clears the queue (alongside the existing turn-cancel).
	// Slash commands keep their existing mid-turn semantics — only
	// plain text is queued.
	queuedPrompts []queuedItem

	// queueClock is a deterministic monotonic counter used to set
	// QueuedAt on each enqueue. Time-based ordering inside one
	// priority bucket relies on this rather than wall-clock so tests
	// stay stable; production runs see a strictly increasing seq.
	queueClock uint64

	// turnBackgrounded — Phase F Ctrl+B (2026-05-12). When the user
	// presses Ctrl+B mid-turn, the active turn keeps running but its
	// streaming output stops mirroring into the visible chat. The
	// streamingText buffer still accumulates so finalizeTurn can
	// flush the full reply once the turn ends; the spinner shrinks
	// to a small "background" status chip. Ctrl+B again
	// foregrounds. finalizeTurn force-fires a desktop notification
	// on backgrounded turns regardless of the 30s gate (the whole
	// reason the user backgrounded was to look away).
	turnBackgrounded bool
	// backgroundedAt tracks when the current turn was backgrounded
	// so the status chip can render "bg 12s" without re-purposing
	// the spinner-start timestamp.
	backgroundedAt time.Time

	// queuePending is set by finalizeTurn after it loaded a queued
	// prompt into the input. The next spinner tick (or any Update
	// pass) calls handleSubmit to actually fire the turn — splitting
	// that across two updates avoids re-entrant runTurnAsync issues
	// where finalizeTurn is mid-cleanup.
	queuePending bool

	// expandedToolID is the A1 ctrl+O mechanism: the **single** tool
	// event currently expanded. Empty string means "nothing expanded"
	// (default). When the user presses ctrl+O:
	//   - with nothing selected, expand the latest row with hidden detail
	//   - repeated presses walk backward through expandable rows, then clear
	// Only one tool event can be expanded at a time, which keeps the
	// viewport height bounded (BUG-A fix).
	//
	// 2026-08-02 P0-1: the legacy expandToolOutputs global toggle has
	// been REMOVED. It used to live here and power ctrl+O, but the
	// global-toggle semantics exploded the viewport on busy transcripts
	// (every tool call suddenly rendered its full body), pushing history
	// out of view. The one-at-a-time semantics above replace it.
	expandedToolID string

	// thinkingDisplay controls how Message{Role:"thinking"} +
	// Message{Role:"redacted_thinking"} render in the transcript.
	// Three values, set via /thinking slash command:
	//   "auto"   (default) — compact live/history preview,
	//                        🔒 placeholder for redacted blocks
	//   "show"   — always expanded, never collapse
	//   "hide"   — skip ALL thinking/redacted_thinking rows entirely
	// Mirrors the spirit of CC's Ctrl+O transcript mode but exposed
	// as an explicit user preference so the user picks how chatty
	// the trace should be on EVERY turn, not per-press.
	thinkingDisplay string

	// stickyBottom controls auto-follow during streaming. true (the
	// default) means new content auto-scrolls into view; user wheel-up
	// or PgUp flips it false; explicit ScrollToBottom / Ctrl+End / new
	// turn submission resets it true. Mirrors claude-code's
	// useVirtualScroll.isSticky semantics — survives transient scroll-
	// past-bottom noise during fast streaming so the bottom keeps
	// following the cursor instead of locking on a stale frame.
	stickyBottom bool

	// lastModeCycle gates Shift+Tab handling against terminal startup
	// bursts that would otherwise cycle modes 3-5 times before the
	// user touches a key.
	lastModeCycle time.Time
	// lastEsc tracks the most recent ESC press so a double-tap within
	// doubleEscWindow clears the input.
	lastEsc time.Time

	// 2026-05-24: sticky-strip selection state (image 67 user feedback
	// — drag-to-select the bottom bypass-mode / tasks region).
	// stripStartY is the row inside the View where the sticky strip's
	// first line lands; set in tui_render.View() so MouseClickMsg /
	// MouseMotionMsg can derive a 0-based line index inside the strip
	// (`msg.Y - stripStartY`). stripPlainLines is the plain (ANSI-
	// stripped) per-line text, sourced from renderStickyTaskStrip's
	// styled output and used as the clipboard payload when a selection
	// is finalised. stripSelStart/End are line indices into
	// stripPlainLines; -1 means "no selection". stripSelDragging tracks
	// whether mouse-down landed in strip and a drag is in progress.
	stripStartY      int
	stripPlainLines  []string
	stripSelStart    int
	stripSelEnd      int
	stripSelDragging bool

	// turnCancel cancels the in-flight turn's ctx (cancellable copy of
	// m.ctx). Set by runTurnAsync, cleared when the turn finishes.
	// Ctrl-C calls it to abort the LLM stream + tool execution while
	// keeping the session alive.
	turnCancel context.CancelFunc
	// lastCtrlC records the last time Ctrl-C was pressed *outside* an
	// active turn, so the second press within ctrlCQuitWindow exits.
	lastCtrlC time.Time

	// savedTermios is the snapshot of the user's terminal state taken
	// at RunTUI startup, made available to handleKey's hard-exit
	// fallback (the 800ms os.Exit timer skips RunTUI's deferred
	// resetTerminal). Without this, mouse-tracking sequences like
	// `\x1b[?1006h` stayed enabled after a Ctrl+C×2 hard exit and
	// every subsequent mouse motion / wheel / click in the user's
	// shell got echoed as raw `<col;row;buttonM` text — image
	// 2026-05-15.
	savedTermios *termSavedState

	// activeScreen is a full-window overlay (e.g. /history). When
	// non-nil, the chat surface is hidden and key events are forwarded
	// to the screen until it reports Done().
	activeScreen screen.Screen

	eventCh chan agent.Event
	doneCh  chan error

	// overlays owns every modal/dialog/popup. New overlays land in
	// internal/tui/overlay/ and Push() onto this stack — see Phase 1
	// of the TUI sub-model refactor (2026-05-01). Old per-overlay
	// boolean flags on Model are getting migrated one by one. /btw
	// is the first migrant.
	overlays *overlay.Stack

	// renderCache memoizes per-message / per-tool-event render output
	// so the View() loop pays glamour cost once per item instead of
	// every spinner tick. WindowSizeMsg invalidates the whole cache;
	// streaming/thinking text is rendered outside the timeline path
	// and never enters the cache. See render_cache.go.
	renderCache *renderCache

	// msgSeq is the monotonic counter behind nextID(). Plain int64
	// (not atomic.Int64 type) on purpose: existing tests copy *Model
	// by value (btw_e2e_test.go) and the new atomic types embed
	// sync/atomic.noCopy which would trip `go vet`. The field is
	// still accessed exclusively via atomic.AddInt64 so concurrent
	// pre-render writers (future tea.Cmd path) stay race-free.
	msgSeq int64

	// wheelAccum accumulates mouse-wheel deltas between forwarded
	// list.ScrollBy calls when ScrollQuantum > 0. A trackpad emits
	// dozens of wheel events per gesture; quantizing into N-line
	// chunks (claude-code SCROLL_QUANTUM=40 inspiration) reduces
	// per-frame churn at the chat-list level. Sign tracks direction
	// (negative = up, positive = down).
	wheelAccum int

	// Mouse click-count tracking — used to detect double/triple
	// clicks for word/line selection on the chat surface. Mirrors
	// crush's chat.go (clickCount + lastClickTime + lastClickX/Y).
	// Within doubleClickThreshold AND clickPosTolerance the count
	// increments; otherwise it resets to 1.
	clickCount    int
	lastClickTime time.Time
	lastClickX    int
	lastClickY    int
}

// doubleClickThreshold is the maximum gap between two MouseClickMsg
// events for them to count as a double-click. crush uses 500ms, macOS
// system-wide default is ~300-500ms. 400ms is the comfortable middle.
const doubleClickThreshold = 400 * time.Millisecond

// clickPosTolerance is the maximum (x, y) drift between consecutive
// clicks for them to still register as the same multi-click. 2 cells
// covers natural finger jitter on a trackpad without being so loose
// that two-finger taps in different rows count as a double-click.
const clickPosTolerance = 2

// chatStartY is the row where chatList's rendering begins inside the
// alt-screen. y=0 is the brand line ("metis · model · mode"); y=1 is
// the separator; y=2 onward is the list. Used to translate viewport
// mouse coordinates into list-relative coordinates.
const chatStartY = 2

// nextID returns a process-stable identifier for a new Message or
// ToolEvent. Format is "<sessionID>-m<seq>" so debug logs can be
// correlated to a session without an external uuid dependency. The
// counter is cleared on every NewModel — IDs are not persisted; if
// the user reloads the session, the rebuilt timeline gets fresh IDs.
func (m *Model) nextID() string {
	return fmt.Sprintf("%s-m%d", m.sessionID, atomic.AddInt64(&m.msgSeq, 1))
}

// ensureIDs lazily assigns IDs to any Message or ToolEvent that
// reached m.messages / m.toolEvents without one. We do it on the
// View() critical path rather than at every append site because the
// codebase has ~70 distinct append points (keybind_submit alone has
// 50+) — threading m.nextID() through each one is mechanical
// boilerplate that adds friction to every new slash command. Lazy
// fill is safe because:
//   - cache keys don't include ID, so a per-frame backfill creates
//     no cache invalidation churn
//   - the only consumers of Message.ID / ToolEvent.ID today are
//     post-View features (cross-pane reference, SSE-reconnect match),
//     all of which run after View() has touched the slice
//   - linear scan of an append-only slice is < 1µs at 100 entries,
//     dwarfed by the glamour render cost the cache is meant to skip
//
// Existing IDs are preserved so the (×N) error-dedupe path keeps the
// same identifier across content rewrites.
func (m *Model) ensureIDs() {
	for i := range m.messages {
		if m.messages[i].ID == "" {
			m.messages[i].ID = m.nextID()
		}
	}
	for i := range m.toolEvents {
		if m.toolEvents[i].ID == "" {
			m.toolEvents[i].ID = m.nextID()
		}
	}
}

const ctrlCQuitWindow = 600 * time.Millisecond

// ============================================================================
// Constructor + entry point
// ============================================================================

func NewModel(ctx context.Context, loop *agent.Loop, cronSvc *agent.CronService, sl *slash.Registry, st *session.Store, sid string, gate *permission.Gate, model, providerName, skillDir string, cfg *config.Config) *Model {
	ti := textarea.New()
	ti.Placeholder = "type a message · /commands · alt+enter newline"
	ti.Focus()
	ti.CharLimit = 8192
	ti.SetWidth(80)
	ti.SetHeight(1)
	// Auto-grow on long input. Without DynamicHeight, a 200-char single-
	// line message squeezes into one row and the visible portion scrolls
	// horizontally — the user can't see what they typed (image #10
	// 2026-05-07: "输入框输入一个超一行的就出现这个图里的情景"). Crush
	// uses the same model: DynamicHeight + Min/MaxHeight, with the
	// outer chrome layer counting wrapped rows automatically because
	// renderInputLine reads the post-wrap textarea View().
	ti.DynamicHeight = true
	ti.MinHeight = 1
	ti.MaxHeight = 10
	ti.ShowLineNumbers = false
	// Disable virtual cursor — bubbles/v2 textarea paints the cursor by
	// SGR-inverting the char under it (cursor.go View() always calls
	// .Reverse(true) when not blinked-off), which produces a green/cyan
	// block on the first placeholder character. claude-code uses the
	// terminal's native cursor; matching that means turning off the
	// inverse paint entirely. SetVirtualCursor(false) puts the cursor
	// in CursorHide mode → cursor.View renders only TextStyle (no
	// reverse), so the placeholder's first char blends with the rest of
	// the dim grey placeholder text. The terminal's own cursor remains
	// at the correct row/col via tea.View bookkeeping.
	ti.SetVirtualCursor(false)
	// Strip the cursor-line background highlight ONLY. bubbles/v2
	// textarea's default CursorLine style is `Background(ANSI 0)` in
	// dark terminals (textarea.go:400) which iTerm2 renders as a deep-
	// blue band. We can't replace CursorLine with an empty style: the
	// placeholder render path (placeholderView, textarea.go:1530-1533)
	// uses CursorLine as the line wrapper for the FIRST placeholder
	// row, so a fully-empty CursorLine breaks placeholder layout (the
	// width-padding `gap` falls through unstyled and bubbletea v2's
	// renderer treats the unstyled trailing whitespace as a separate
	// line region — that's why the prior naïve override produced a
	// duplicate placeholder strip below the input).
	//
	// UnsetBackground keeps Inline/Foreground/etc but drops the
	// background color so the row blends with the surrounding chrome.
	{
		ts := ti.Styles()
		ts.Focused.CursorLine = ts.Focused.CursorLine.UnsetBackground()
		ts.Focused.CursorLineNumber = ts.Focused.CursorLineNumber.UnsetBackground()
		ts.Blurred.CursorLine = ts.Blurred.CursorLine.UnsetBackground()
		ts.Blurred.CursorLineNumber = ts.Blurred.CursorLineNumber.UnsetBackground()
		ti.SetStyles(ts)
	}
	// SetPromptFunc lets textarea handle the per-line prompt itself —
	// "> " on the first row, "  " on continuation rows. Doing this in
	// textarea (instead of post-processing its View() output) avoids
	// collisions with textarea's internal height/cursor accounting,
	// which otherwise rendered the typed `/` on row 2 instead of row 1.
	// v2: textarea's prompt-func signature changed from
	// `func(lineIdx int) string` to `func(textarea.PromptInfo) string`
	// where PromptInfo bundles LineNumber + Focused.
	ti.SetPromptFunc(2, func(info textarea.PromptInfo) string {
		// claude-code uses ">" (ASCII greater-than) — flatter look that
		// fits the no-border input style.
		if info.LineNumber == 0 {
			return "> "
		}
		return "  "
	})

	// chatList replaces bubbles/viewport.Model for the chat surface.
	// Width/Height get sized on the first WindowSizeMsg; default 80×20
	// keeps test code that constructs a Model literal usable.
	cl := list.NewList()
	cl.SetSize(80, 20)
	cl.SetGap(0) // assistant/user message renderers already include trailing newlines
	// Wheel step is config-tunable now (default 1 = pixel-precise).
	// A trackpad fires many wheel events per gesture; with the bubbletea
	// default of 3 the transcript jumps too far per detent. Users who
	// want browser-like jumpy scroll can set
	// `[ui.performance].mouse_wheel_lines = 3` or env METIS_MOUSE_WHEEL_LINES=3.
	cl.SetMouseWheelDelta(mouseWheelLines())
	// claude-code-style hard cap on physically mounted items. Default
	// 0 (unbounded) preserves existing behavior; set
	// `[ui.performance].max_mounted_items = 300` to enable. Older
	// messages still live in m.messages — they just don't reach the
	// chatList until the user scrolls back into them (future work).
	if mm := perfConfig().MaxMountedItems; mm > 0 {
		cl.SetMaxMounted(mm)
	}

	// Render cache picks up SlowRenderMs / StatsLogEvery from the
	// active perf config; both fall back to the cache's own defaults
	// when the snapshot is zero (tests / fresh installs without TOML).
	pc := perfConfig()
	thinkingDisplay := configuredThinkingDisplay(cfg)

	mdl := &Model{
		ctx:        ctx,
		loop:       loop,
		cronSvc:    cronSvc,
		gate:       gate,
		slash:      sl,
		session:    st,
		sessionID:  sid,
		baseSystem: loop.System,
		baseSystemSections: append([]llm.SystemSection(nil),
			loop.SystemSections...),
		model:        model,
		providerName: providerName,
		skillDir:     skillDir,
		cfg:          cfg,
		cmds:         BuildREPLCommands(),
		startTime:    time.Now(),
		// Default terminal size so the FIRST frame paints a real banner
		// instead of a blank screen. bubbletea delivers WindowSizeMsg
		// asynchronously — the first View() runs before it arrives, and
		// under tmux that message can lag a SIGWINCH by seconds. Every
		// width-driven renderer (banner, input, status bar) collapses to
		// empty output at width 0, so without a default the user stares
		// at a blank screen and thinks metis hung (2026-06-15). 80x24 is
		// the universal terminal fallback; the real WindowSizeMsg
		// re-renders at the true size a frame later.
		width:       80,
		height:      24,
		eventCh:     make(chan agent.Event, eventBufferSize()),
		doneCh:      make(chan error, 1),
		overlays:    overlay.New(),
		renderCache: newRenderCache(pc.SlowRenderMs, pc.StatsLogEvery),
		showBanner:  true,
		// Sticky-strip selection state — -1 = no selection. Updated by
		// MouseClickMsg / MouseMotionMsg / MouseReleaseMsg when click
		// lands in the strip area (msg.Y >= stripStartY).
		stripSelStart:   -1,
		stripSelEnd:     -1,
		firstRender:     true,
		input:           ti,
		chatList:        cl,
		stickyBottom:    true,
		thinkingDisplay: thinkingDisplay,
		// 4-level permission ask, matching claude-code's pattern:
		//   y — allow this once
		//   a — allow always (whitelist this tool for the session)
		//   n — deny this once (turn keeps going, tool returns error)
		//   c — cancel: deny + abort the whole turn
		permChoices: []permChoice{
			{Label: "Yes", Key: "y"},
			{Label: "Yes, always", Key: "a"},
			{Label: "No", Key: "n"},
			{Label: "Cancel turn", Key: "c"},
		},
		histDirectIdx:   -1, // not navigating yet — first ↑ jumps to histAll[0]
		toolArgsStreams: make(map[string]toolArgsStreamState),
	}
	mdl.historyCursor = session.NewHistoryCursor(loop.History())
	// Hydrate sessionTitle from the on-disk header so a resumed session
	// (e.g. metis --resume <id> where the previous run had run /rename
	// "foo") shows "foo" in the terminal tab on first frame, not just
	// after the next /rename. Best-effort: nil store / nil header / read
	// error are all soft fails — leave sessionTitle empty so View()
	// falls back to the plain "metis" window title.
	if st != nil && sid != "" {
		if hdr, _, err := st.LoadHeader(sid); err == nil && hdr != nil {
			mdl.sessionTitle = hdr.Title
		}
	}
	if pendingUpdateNotice != "" {
		// Surface the update notice as the first info row so the user
		// sees it inside alt-screen rather than having it swallowed
		// when bubbletea swaps buffers.
		mdl.messages = append(mdl.messages, Message{
			Role:      "info",
			Content:   "[update] " + pendingUpdateNotice,
			Timestamp: time.Now(),
		})
		pendingUpdateNotice = ""
	}
	// Hydrate the chat surface from loop.Messages on resumed sessions
	// (2026-05-15 fix). ApplyResume restores loop.Messages but never
	// touches m.messages — without this call, `metis --resume <id>`
	// opens a blank chat even though the LLM has full context.
	// No-op for fresh sessions (loop.Messages is empty).
	mdl.hydrateFromLoopHistory()
	// Restore the persisted cumulative cost so a resumed session's /cost
	// reflects pre-resume spend — the conversation hydrates above, but the
	// token tally would otherwise start at zero (image #5). No-op for fresh
	// sessions or ones with no cost sidecar yet.
	if st != nil && sid != "" {
		if c, ok, _ := st.ReadCost(sid); ok {
			mdl.totalTokens.Restore(c.InputTokens, c.OutputTokens, c.CacheCreateTokens, c.CacheReadTokens)
		}
	}
	return mdl
}

func configuredThinkingDisplay(cfg *config.Config) string {
	if cfg != nil {
		mode := strings.ToLower(strings.TrimSpace(cfg.UI.ThinkingDisplay))
		switch mode {
		case "show", "hide", "auto":
			return mode
		}
	}
	return "auto"
}

// SetExternalHooks attaches optional callbacks for features whose state
// lives in the runtime layer (additional dirs, /btw side question, ...).
// Safe to call before or after RunTUI; nil hooks degrade gracefully.
func (m *Model) SetExternalHooks(h ExternalHooks) {
	m.ext = h
}

// RunTUI starts the terminal UI. If hooks is non-nil it is attached to
// the underlying Model before the program runs.
func RunTUI(ctx context.Context, loop *agent.Loop, cronSvc *agent.CronService, sl *slash.Registry, st *session.Store, sid string, gate *permission.Gate, model, providerName, skillDir string, cfg *config.Config, forceBanner bool, hooks ...ExternalHooks) error {
	m := NewModel(ctx, loop, cronSvc, sl, st, sid, gate, model, providerName, skillDir, cfg)
	if len(hooks) > 0 {
		m.SetExternalHooks(hooks[0])
	}
	if forceBanner {
		m.firstRender = true
	}
	// Early-input forwarding via tea.WithInput is currently disabled.
	// bubbletea v2.0.6 (alpha) does not switch the terminal into raw
	// mode when a custom io.Reader is supplied via WithInput — it
	// only does that for its default stdin path. With a custom reader
	// the terminal stays canonical/echo, so typed characters are
	// echoed straight to the cursor by the terminal itself instead
	// of reaching textarea, and bubbletea sees nothing until Enter.
	// We therefore drop the cold-start keystroke buffer; the worst
	// case is the user loses ≤100 ms of pre-typed input. Live typing
	// after bubbletea starts is unaffected. Re-enable when v2 either
	// raw-modes custom readers or exposes a separate early-input API.
	opts := []tea.ProgramOption{tea.WithContext(ctx)}
	_ = earlyInputReader
	// v2: WithAltScreen / WithMouseCellMotion options are gone — those
	// terminal modes are now declared per-frame in View() via
	// tea.View.AltScreen and tea.View.MouseMode. See tui_render.go.
	//
	// Snapshot termios BEFORE bubbletea takes over so the deferred
	// resetTerminal can restore exactly the state the shell was in.
	// bubbletea v2.0.6's Quit cleanup occasionally misses kitty-
	// keyboard disable, leaving Ctrl+C echoing as ^[[99;5u in the
	// shell — the deferred reset is the bullet-proof fallback.
	//
	// ALSO stamp it onto the Model so handleKey's hard-exit timer
	// (the goroutine that fires os.Exit after 800ms when the polite
	// tea.Quit shutdown stalls) can run the same reset before the
	// process disappears. Without that, mouse-tracking sequences
	// stay enabled in the user's shell after Ctrl+C×2 hard exit.
	saved := snapshotTerminal()
	m.savedTermios = saved
	defer resetTerminal(saved)

	p := tea.NewProgram(m, opts...)
	_, err := p.Run()
	// Persist the final mode + interactive approvals even when the user never
	// switched sessions in-process; otherwise a later --resume has nothing to
	// restore from the header's AlwaysAllow field.
	if persistErr := m.persistActiveSessionState(); persistErr != nil {
		err = errors.Join(err, persistErr)
	}
	// Claude-code-style goodbye hint — print AFTER bubbletea releases
	// alt-screen so it lands in the user's normal scrollback. Tells
	// them how to come back to this exact session next time. Skipped
	// on error (the error itself is the priority message) and when sid
	// is empty (e.g. the user quit during the auth wizard before any
	// session was created).
	if err == nil && m.sessionID != "" {
		printResumeHint(m.session, m.sessionID)
	}
	return err
}

// printResumeHint surfaces a dim "next time, run this" hint after a
// clean chat exit. Format mirrors claude-code's quit affordance
// (user reference image 2026-05-08) but adapted for metis's
// UUID-only --resume contract:
//
//	Resume "<title>":               ← only when /rename has been used
//	  metis --resume <full-uuid>
//
//	Resume this session with:       ← fallback when title is empty
//	  metis --resume <full-uuid>
//
// We can't put the title on the command line the way claude-code does
// (`claude --resume "foo"`) because metis's resume requires the
// canonical UUID by design — see internal/runtime/resume.go for the
// rationale (prefix/title resolution was tried and reverted). Instead
// we surface the title in the lead line so the user sees a friendly
// label and the command line stays copy-pasteable verbatim.
//
// Both lines in dim gray (ANSI 2;38;5;245). stderr so piped stdout
// consumers don't see the human chrome. store may be nil (e.g. quit
// during auth wizard before session boot completes); in that case we
// silently skip the title lookup and fall back to the plain hint.
func printResumeHint(store *session.Store, sid string) {
	dim := "\x1b[2;38;5;245m"
	reset := "\x1b[0m"
	title := ""
	if store != nil {
		if hdr, _, err := store.LoadHeader(sid); err == nil && hdr != nil {
			title = strings.TrimSpace(hdr.Title)
		}
	}
	if title != "" {
		fmt.Fprintf(os.Stderr, "\n%sResume %q:%s\n", dim, title, reset)
	} else {
		fmt.Fprintf(os.Stderr, "\n%sResume this session with:%s\n", dim, reset)
	}
	fmt.Fprintf(os.Stderr, "%s  metis --resume %s%s\n", dim, sid, reset)
}

// earlyInputReader is set by SetEarlyInputReader from main.go before
// RunTUI starts. Package-level so the wiring stays simple — the
// alternative is plumbing it through RunTUI's already-long signature.
// Reset to nil after RunTUI consumes it (so subsequent runs don't see
// stale buffered bytes).
var earlyInputReader io.Reader

// SetEarlyInputReader hands a pre-populated input reader to the next
// RunTUI call. main.go calls this with runtime.EarlyInput.Reader()
// after the EarlyInput's Stop() has restored terminal mode.
func SetEarlyInputReader(r io.Reader) { earlyInputReader = r }

// pendingUpdateNotice is set by SetPendingUpdateNotice from main.go's
// maybeNotifyUpdate when a newer release is detected. Stashed here as
// package-level state because writing the notice to stderr direct
// gets swallowed when bubbletea swaps to alt-screen — instead the
// next NewModel pulls it and surfaces an info row inside the chat
// transcript so the user sees the "metis vX.Y.Z available" hint.
var pendingUpdateNotice string

// SetPendingUpdateNotice queues an update-available notice to be
// shown as the first info row in the next TUI session. Cleared on
// consumption.
func SetPendingUpdateNotice(notice string) { pendingUpdateNotice = notice }
