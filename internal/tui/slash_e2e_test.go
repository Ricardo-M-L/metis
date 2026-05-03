package tui

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	"github.com/Ricardo-M-L/metis/internal/tui/list"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tui/overlay"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// TestSlashE2E_TableDriven submits each user-visible slash command through
// the real keybind dispatcher and asserts the resulting Message log
// contains a stable substring proving the right handler ran.
//
// Important context: metis has TWO command registries that handleSubmit
// consults in order:
//  1. m.cmds  — REPLCommandRegistry (BuildREPLCommands), older path
//  2. m.slash — slash.Registry (RegisterAll), Signal-based path
//
// REPL commands win when both define the same name. Many commands added
// in the 2026-05-01 audit (/cost, /diff, /doctor, /vim, /export, /theme,
// /effort, /context, /rename) collide with pre-existing REPL commands —
// the SignalXXX wiring for those is dead code, but the command itself
// still works because the REPL handler runs. This test verifies behavior
// regardless of which path took it; expectations are aligned to the
// path that actually runs.
func TestSlashE2E_TableDriven(t *testing.T) {
	cases := []struct {
		input    string
		wantSubs []string
		owner    string // "repl" / "slash" — for documentation only
	}{
		// REPL-owned (pre-existing) ----------------------------------------
		{"/cost", []string{"Session Cost", "input tokens", "output tokens", "est. cost"}, "repl"},
		{"/usage", []string{"rate limit"}, "repl"},
		{"/doctor", []string{"Metis Doctor", "config", "git"}, "repl"},
		{"/vim", []string{"vim mode:"}, "repl"},
		{"/theme", []string{"Theme", "◀", "▶"}, "widget"}, // Phase C4: opens cycle widget
		{"/effort", []string{"Speed", "Intelligence", "▲"}, "widget"}, // Phase C1: opens slider widget
		{"/effort high", []string{"effort: high"}, "repl"},
		{"/context", []string{"Context Window", "in last call", "tokens"}, "repl"},
		{"/export", []string{"exported", "messages to"}, "repl"},

		// Slash-owned (added in 2026-05-01 audit) --------------------------
		{"/keybindings", []string{"Keybindings", "Ctrl-C", "Esc"}, "slash"},
		{"/keys", []string{"Ctrl-C"}, "slash"}, // alias
		{"/permissions", []string{"Mode", "Rules", "◀", "▶"}, "widget"},     // Phase C5: opens editor
		{"/perms", []string{"Mode", "Rules", "◀", "▶"}, "widget"},          // Phase C5: same widget
		{"/hooks", []string{"hooks"}, "slash"},
		{"/stats", []string{"Session Stats", "user turns", "tool calls"}, "slash"},
		{"/release-notes", []string{"metis"}, "slash"},
		{"/changelog", []string{"metis"}, "slash"},
		{"/resume", []string{"--resume"}, "slash"},
		{"/pr_comments 42", []string{"/pr_comments"}, "slash"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			m := newSlashTestModel(t)
			before := len(m.messages)

			m.input.SetValue(tc.input)
			pressEnter(t, m)

			// Phase A migration: many of these commands now open a
			// BodyScreen modal overlay instead of inlining into the
			// chat. Look in either place for the expected substring
			// so this test stays valid across the migration.
			var output string
			switch {
			case m.activeScreen != nil:
				output = m.activeScreen.View()
			case len(m.messages) > before:
				output = m.messages[len(m.messages)-1].Content
			default:
				t.Fatalf("submitting %q produced neither a screen overlay nor a chat message (owner=%s)", tc.input, tc.owner)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(output, sub) {
					t.Errorf("output for %q missing %q\n----full output----\n%s\n----", tc.input, sub, output)
				}
			}
		})
	}
}

// /vim has explicit on/off/toggle subargs (the REPL handler controls
// vimModeState directly).
func TestSlashE2E_VimOnOff(t *testing.T) {
	vimModeState = vimOff
	defer func() { vimModeState = vimOff }()

	m := newSlashTestModel(t)

	m.input.SetValue("/vim on")
	pressEnter(t, m)
	last := m.messages[len(m.messages)-1].Content
	if !strings.Contains(last, "vim mode:") {
		t.Errorf("/vim on: %q", last)
	}
	if vimModeState == vimOff {
		t.Errorf("/vim on should flip vimModeState off→insert/normal, got %q", vimModeState)
	}

	m.input.SetValue("/vim off")
	pressEnter(t, m)
	last = m.messages[len(m.messages)-1].Content
	if !strings.Contains(last, "off") {
		t.Errorf("/vim off response: %q", last)
	}
	if vimModeState != vimOff {
		t.Errorf("/vim off should reset state, got %q", vimModeState)
	}
}

// /tag goes through SignalTag (REPL has no /tag) — verify the Slash path.
func TestSlashE2E_Tag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)

	m := newSlashTestModel(t)

	// /tag needs an active session store.
	store, err := session.NewStore(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	sid := store.NewSessionID()
	if err := store.WriteHeader(sid, "test-model", "test-system"); err != nil {
		t.Fatal(err)
	}
	m.session = store
	m.sessionID = sid

	m.input.SetValue("/tag mylabel")
	pressEnter(t, m)

	last := m.messages[len(m.messages)-1].Content
	if !strings.Contains(last, "tagged") {
		t.Errorf("/tag output: %q", last)
	}

	tagFile := filepath.Join(home, "sessions", "tags", sid+".txt")
	b, err := os.ReadFile(tagFile)
	if err != nil {
		t.Fatalf("expected tag file at %s: %v", tagFile, err)
	}
	if !strings.Contains(string(b), "mylabel") {
		t.Errorf("tag file should contain 'mylabel', got %q", string(b))
	}
}

// /add-dir + /rm-dir + bare /add-dir (= list) — ExternalHooks dispatch.
func TestSlashE2E_AddDirAndList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	target := t.TempDir()

	addedPaths := []string{}
	listed := 0

	m := newSlashTestModel(t)
	m.SetExternalHooks(ExternalHooks{
		DirAdd:    func(p string, persist bool) error { addedPaths = append(addedPaths, p); return nil },
		DirRemove: func(p string) error { return nil },
		DirList:   func() []string { listed++; return addedPaths },
	})

	m.input.SetValue("/add-dir " + target)
	pressEnter(t, m)
	if len(addedPaths) != 1 || addedPaths[0] != target {
		t.Errorf("DirAdd never received %q (got %v)", target, addedPaths)
	}

	m.input.SetValue("/add-dir")
	pressEnter(t, m)
	if listed != 1 {
		t.Errorf("DirList should fire once on bare /add-dir, got %d", listed)
	}
	if !strings.Contains(m.messages[len(m.messages)-1].Content, target) {
		t.Errorf("list output should mention %q", target)
	}

	m.input.SetValue("/rm-dir " + target)
	pressEnter(t, m)
	if !strings.Contains(m.messages[len(m.messages)-1].Content, "removed") {
		t.Errorf("/rm-dir output: %q", m.messages[len(m.messages)-1].Content)
	}
}

// /batch rewrites the user prompt with the full Research/Plan/Execute
// contract. This test stops at "the rewritten text appears in the loop
// history"; it does NOT let the agent loop actually fire.
func TestSlashE2E_BatchRewritesPrompt(t *testing.T) {
	m := newSlashTestModelWithLoop(t)
	m.input.SetValue("/batch rename foo to bar")

	// pressEnter triggers handleSubmit which falls through to AppendUser +
	// runTurnAsync. We don't wait for the goroutine; the synchronous part
	// of AppendUser already wrote our message into history.
	pressEnter(t, m)

	// runTurnAsync is firing in a goroutine using fakeProvider.Stream which
	// returns (nil, nil) and would NPE. Cancel the context so the goroutine
	// exits cleanly when its select sees ctx.Done.
	if cancel, ok := m.ctx.Value(cancelKey{}).(context.CancelFunc); ok {
		cancel()
	}
	// Give the goroutine a moment to bail out on the cancelled context so
	// the test's exit doesn't race with `panic in goroutine`. The Loop
	// guards Stream calls against nil returns.
	time.Sleep(50 * time.Millisecond)

	// fakeProvider may have already appended an assistant turn; walk the
	// history backwards and grab the latest "user" message — that's the
	// one /batch rewrote.
	hist := m.loop.History()
	body := ""
	for i := len(hist) - 1; i >= 0; i-- {
		if string(hist[i].Role) == "user" {
			for _, c := range hist[i].Content {
				if c.Type == "text" {
					body += c.Text
				}
			}
			break
		}
	}
	if body == "" {
		t.Fatalf("/batch should append a user message; history=%+v", hist)
	}
	for _, sub := range []string{"PHASE 1", "PHASE 2", "PHASE 3", "rename foo to bar"} {
		if !strings.Contains(body, sub) {
			t.Errorf("/batch user message missing %q", sub)
		}
	}
}

// ============================================================================
// helpers
// ============================================================================

type cancelKey struct{}

// fakeProvider satisfies pkg/provider.Provider for tests that need a Loop
// but never actually want to hit an LLM. Complete/Stream return an
// immediate-EOF reader + a "no content" response so the Loop terminates
// the turn gracefully rather than NPE'ing on a nil stream.
type fakeProvider struct{}

func (fakeProvider) Name() string { return "fake" }
func (fakeProvider) Complete(_ context.Context, _ provider.Request) (*provider.Response, error) {
	return &provider.Response{StopReason: "end_turn"}, nil
}
func (fakeProvider) Stream(_ context.Context, _ provider.Request) (provider.StreamReader, error) {
	return &fakeStream{}, nil
}
func (fakeProvider) MaxContextTokens() int { return 200_000 }

// fakeStream emits a single message_stop event then EOF so the agent
// loop's consumeStream sees a clean turn-ended signal.
type fakeStream struct{ done bool }

func (s *fakeStream) Recv() (provider.StreamEvent, error) {
	if s.done {
		return provider.StreamEvent{}, io.EOF
	}
	s.done = true
	return provider.StreamEvent{Type: "message_stop", StopReason: "end_turn"}, nil
}
func (s *fakeStream) Close() error { return nil }

// newSlashTestModel builds the bare minimum Model for slash dispatch.
// Sets METIS_HOME to t.TempDir() so file-touching commands stay sandboxed.
func newSlashTestModel(t *testing.T) *Model {
	t.Helper()
	if os.Getenv("METIS_HOME") == "" {
		t.Setenv("METIS_HOME", t.TempDir())
	}

	ti := textarea.New()
	ti.SetWidth(80)
	ti.Focus()
	cl := list.NewList()
	cl.SetSize(78, 20)

	slashReg := slash.NewRegistry()
	slash.RegisterAll(slashReg, nil)

	m := &Model{
		ctx:         context.Background(),
		gate:        permission.New(permission.ModeAuto),
		slash:       slashReg,
		cmds:        BuildREPLCommands(),
		startTime:   time.Now(),
		input:       ti,
		chatList:    cl,
		overlays:    overlay.New(),
		width:       100,
		height:      40,
		firstRender: false,
		showBanner:  false,
		loop:        agent.NewLoop(fakeProvider{}, tools.NewRegistry(), permission.New(permission.ModeAuto), nil, "sys", 10),
		model:       "claude-sonnet-4-6",
		cfg:         &config.Config{},
	}
	m.messages = append(m.messages, Message{Role: "info", Content: "(test session)", Timestamp: time.Now()})
	return m
}

// newSlashTestModelWithLoop mirrors newSlashTestModel but wraps ctx in a
// cancelable so /batch tests can cancel the spawned turn before its
// fakeProvider.Stream returns (nil, nil) and the Loop NPEs.
func newSlashTestModelWithLoop(t *testing.T) *Model {
	t.Helper()
	m := newSlashTestModel(t)
	cctx, cancel := context.WithCancel(m.ctx)
	cctx = context.WithValue(cctx, cancelKey{}, cancel)
	m.ctx = cctx
	return m
}
