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
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// ============================================================================
// Public types
// ============================================================================

type Message struct {
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
	imagePaste    map[int]string // N -> /path/to/cached.png
	imageCounter  int            // 1-based; matches the visible #N

	// input is the chat-surface multi-line editor. textarea (instead of
	// textinput) lets the user paste multi-line code and split prompts
	// across rows with Alt+Enter / Ctrl+J. Enter still submits — handleKey
	// intercepts KeyEnter before it reaches textarea.
	input textarea.Model
	// viewport scrolls the message log. PgUp/PgDn and the mouse wheel
	// both work out of the box.
	viewport      viewport.Model
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
	showHistory   bool
	histAll       []string // newest-first dedup'd input strings
	histFilter    string
	histCursor    int
	histMatched   []string // subset of histAll that fuzzy-matches histFilter

	// @-mention dropdown state. Tracked separately from the slash
	// palette so an in-progress `@xxx` filter doesn't fight the slash
	// palette's `palFilter` for the same buffer. Recomputed on every
	// key by `updateAtMention()` (called from the textarea-update
	// codepath), so we don't have to drive it from a dedicated key
	// handler — it just appears when the cursor is in an `@xxx` token.
	atActive   bool
	atFilter   string
	atCursor   int      // selected row in atMatched (0-based)
	atMatched  []string // current fuzzy-matched file paths

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

	// lastViewportLen tracks how many "cells" of content we last gave
	// the viewport. When it grows we auto-GotoBottom so new messages
	// are visible; when the user scrolled up to read history we avoid
	// yanking them back down on every redraw.
	lastViewportLen int

	// activeScreen is a full-window overlay (e.g. /history). When
	// non-nil, the chat surface is hidden and key events are forwarded
	// to the screen until it reports Done().
	activeScreen screen.Screen

	eventCh chan agent.Event
	doneCh  chan error
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

	vp := viewport.New(80, 20) // dimensions get fixed up on first WindowSizeMsg
	vp.MouseWheelEnabled = true
	// Wheel step is config-tunable now (default 1 = pixel-precise).
	// A trackpad fires many wheel events per gesture; with the bubbletea
	// default of 3 the transcript jumps too far per detent. Users who
	// want browser-like jumpy scroll can set
	// `[ui.performance].mouse_wheel_lines = 3` or env METIS_MOUSE_WHEEL_LINES=3.
	vp.MouseWheelDelta = mouseWheelLines()

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
		showBanner:  true,
		firstRender: true,
		input:       ti,
		viewport:    vp,
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

// RunTUI starts the terminal UI.
func RunTUI(ctx context.Context, loop *agent.Loop, sl *slash.Registry, st *session.Store, sid string, gate *permission.Gate, model, skillDir string, cfg *config.Config, forceBanner bool) error {
	m := NewModel(ctx, loop, sl, st, sid, gate, model, skillDir, cfg)
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
