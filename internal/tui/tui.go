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
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
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

// SubAgentInfo is one in-flight sub-agent. Used for status-bar pill
// visualization; rendered as "◇ Name". Populated by handleAgentEvent
// when an Agent tool call fires.
type SubAgentInfo struct {
	ID     string
	Name   string
	Status string // running | completed | failed
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
}

type Model struct {
	ctx       context.Context
	loop      *agent.Loop
	gate      *permission.Gate
	slash     *slash.Registry
	session   *session.Store
	sessionID string
	model     string
	skillDir  string
	cmds      *REPLCommandRegistry
	ext       ExternalHooks
	// cfg is the loaded ~/.metis/config.toml. The TUI reaches into a
	// few sub-sections directly (Tools.Bash for !bash-mode shell
	// settings, Channels for SendMessage routing, etc.). Storing the
	// whole Config keeps Model self-sufficient — feature additions
	// don't need to thread new params through NewModel each time.
	cfg *config.Config

	messages   []Message
	toolEvents []ToolEvent
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
	// expandToolOutputs is the global "show full tool output" toggle
	// (claude-code's ctrl+o). When false (default), Edit diffs cap at
	// 20 lines, Bash output at 5 lines. When true the user gets the
	// full content.
	expandToolOutputs bool

	// lastModeCycle gates Shift+Tab handling against terminal startup
	// bursts that would otherwise cycle modes 3-5 times before the
	// user touches a key.
	lastModeCycle time.Time
	// lastEsc tracks the most recent ESC press so a double-tap within
	// doubleEscWindow clears the input.
	lastEsc time.Time

	// turnCancel cancels the in-flight turn's ctx (cancellable copy of
	// m.ctx). Set by runTurnAsync, cleared when the turn finishes.
	// Ctrl-C calls it to abort the LLM stream + tool execution while
	// keeping the session alive.
	turnCancel context.CancelFunc
	// lastCtrlC records the last time Ctrl-C was pressed *outside* an
	// active turn, so the second press within ctrlCQuitWindow exits.
	lastCtrlC time.Time

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
}

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

func NewModel(ctx context.Context, loop *agent.Loop, sl *slash.Registry, st *session.Store, sid string, gate *permission.Gate, model, skillDir string, cfg *config.Config) *Model {
	ti := textarea.New()
	ti.Placeholder = "type a message · /commands · alt+enter newline"
	ti.Focus()
	ti.CharLimit = 8192
	ti.SetWidth(80)
	ti.SetHeight(1)
	ti.MaxHeight = 5
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

	mdl := &Model{
		ctx:         ctx,
		loop:        loop,
		gate:        gate,
		slash:       sl,
		session:     st,
		sessionID:   sid,
		model:       model,
		skillDir:    skillDir,
		cfg:         cfg,
		cmds:        BuildREPLCommands(),
		startTime:   time.Now(),
		eventCh:     make(chan agent.Event, eventBufferSize()),
		doneCh:      make(chan error, 1),
		overlays:    overlay.New(),
		renderCache: newRenderCache(pc.SlowRenderMs, pc.StatsLogEvery),
		showBanner:  true,
		firstRender: true,
		input:       ti,
		chatList:    cl,
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
		histDirectIdx: -1, // not navigating yet — first ↑ jumps to histAll[0]
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
	return mdl
}

// SetExternalHooks attaches optional callbacks for features whose state
// lives in the runtime layer (additional dirs, /btw side question, ...).
// Safe to call before or after RunTUI; nil hooks degrade gracefully.
func (m *Model) SetExternalHooks(h ExternalHooks) {
	m.ext = h
}

// RunTUI starts the terminal UI. If hooks is non-nil it is attached to
// the underlying Model before the program runs.
func RunTUI(ctx context.Context, loop *agent.Loop, sl *slash.Registry, st *session.Store, sid string, gate *permission.Gate, model, skillDir string, cfg *config.Config, forceBanner bool, hooks ...ExternalHooks) error {
	m := NewModel(ctx, loop, sl, st, sid, gate, model, skillDir, cfg)
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
	saved := snapshotTerminal()
	defer resetTerminal(saved)

	p := tea.NewProgram(m, opts...)
	_, err := p.Run()
	return err
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
