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
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

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
	showHistory bool
	histAll     []string // newest-first dedup'd input strings
	histFilter  string
	histCursor  int
	histMatched []string // subset of histAll that fuzzy-matches histFilter

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
	// SetPromptFunc lets textarea handle the per-line prompt itself —
	// "> " on the first row, "  " on continuation rows. Doing this in
	// textarea (instead of post-processing its View() output) avoids
	// collisions with textarea's internal height/cursor accounting,
	// which otherwise rendered the typed `/` on row 2 instead of row 1.
	ti.SetPromptFunc(2, func(lineIdx int) string {
		// claude-code uses ">" (ASCII greater-than) — flatter look that
		// fits the no-border input style.
		if lineIdx == 0 {
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

	// Render cache picks up SlowRenderMs / StatsLogEvery from the
	// active perf config; both fall back to the cache's own defaults
	// when the snapshot is zero (tests / fresh installs without TOML).
	pc := perfConfig()

	return &Model{
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
	}
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
	p := tea.NewProgram(m,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		// Enable mouse wheel events so scrolling the viewport works
		// without keyboard shortcuts. Cell-motion mode is the lighter
		// of the two — only sends events on click/wheel, not on hover,
		// so it doesn't drown the terminal in motion packets.
		tea.WithMouseCellMotion(),
	)
	_, err := p.Run()
	return err
}
