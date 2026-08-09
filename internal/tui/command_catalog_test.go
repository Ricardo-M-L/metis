package tui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

func TestCommandCatalogIncludesSlashOnlyAndCustomCommands(t *testing.T) {
	m := newSlashTestModel(t)
	m.slash.Register(slash.Cmd{
		Name:        "project-check",
		Description: "run the project check",
		Custom:      true,
		Handler: func(string) (string, slash.Signal) {
			return "check", slash.SignalCustomPrompt
		},
	})

	got := map[string]int{}
	for _, cmd := range m.commandCatalog() {
		got[cmd.Name]++
	}
	for _, name := range []string{"help", "new", "undo", "review", "project-check"} {
		if got[name] != 1 {
			t.Fatalf("catalog count for %q = %d, want 1", name, got[name])
		}
	}

	m.palFilter = "project-c"
	m.matchCommands()
	if len(m.palMatched) == 0 || m.palMatched[0].Name != "project-check" {
		t.Fatalf("custom slash missing from palette: %+v", m.palMatched)
	}
}

func TestToolsetsAliasesCanonicalToolsPicker(t *testing.T) {
	m := newSlashTestModel(t)
	if cmd := m.cmds.Get("toolsets"); cmd == nil || cmd.Name != "tools" {
		t.Fatalf("REPL /toolsets alias = %+v, want canonical /tools", cmd)
	}
	handled, _, sig, _ := m.slash.Parse("/toolsets")
	if !handled || sig != slash.SignalTools {
		t.Fatalf("slash /toolsets routed to handled=%v signal=%v, want SignalTools", handled, sig)
	}

	m.input.SetValue("/toolsets")
	pressEnter(t, m)
	if _, ok := m.activeScreen.(*screen.PickerScreen); !ok {
		t.Fatalf("/toolsets should open the /tools picker, got %T", m.activeScreen)
	}
}

// Claude Code exposes submit as the Enter-bound `chat:submit` action, not as a
// slash command. Keep it out of both Metis command registries so the palette
// does not advertise a command with no meaningful slash semantics.
func TestSubmitIsAKeyActionNotSlashCommand(t *testing.T) {
	m := newSlashTestModel(t)
	if cmd := m.cmds.Get("submit"); cmd != nil {
		t.Fatalf("REPL unexpectedly exposes /submit: %+v", cmd)
	}
	if cmd, ok := m.slash.Get("submit"); ok || cmd != nil {
		t.Fatalf("slash registry unexpectedly exposes /submit: %+v", cmd)
	}
	for _, cmd := range m.commandCatalog() {
		if cmd.Name == "submit" {
			t.Fatal("palette unexpectedly advertises /submit")
		}
	}
}

func TestHelpUsesRealCustomCommandsNotSkills(t *testing.T) {
	m := newSlashTestModel(t)
	m.slash.Register(slash.Cmd{
		Name:        "release-check",
		Description: "validate a release",
		Custom:      true,
		Handler: func(string) (string, slash.Signal) {
			return "release", slash.SignalCustomPrompt
		},
	})

	var text strings.Builder
	for _, row := range m.helpCustomCommandsRows() {
		text.WriteString(row.Key)
		text.WriteString(" ")
		text.WriteString(row.Value)
		text.WriteString("\n")
	}
	if !strings.Contains(text.String(), "/release-check") {
		t.Fatalf("custom help missing real slash command:\n%s", text.String())
	}
	if strings.Contains(text.String(), "SKILL.md") {
		t.Fatalf("custom-command help still advertises skills as slash commands:\n%s", text.String())
	}
}

func TestResetAliasBelongsToNewSessionNotClear(t *testing.T) {
	m := newSlashTestModel(t)
	if got := m.cmds.Get("reset"); got != nil {
		t.Fatalf("REPL /clear must not shadow slash /reset alias: %+v", got)
	}
	handled, _, sig, _ := m.slash.Parse("/reset")
	if !handled || sig != slash.SignalNew {
		t.Fatalf("/reset routed to handled=%v signal=%v, want SignalNew", handled, sig)
	}
}

func TestRichSlashHandlersOwnConflictingTUICommands(t *testing.T) {
	m := newSlashTestModel(t)
	for _, name := range []string{"memory", "doctor"} {
		cmd := m.cmds.Get(name)
		if cmd == nil {
			t.Fatalf("test fixture missing legacy REPL command %q", name)
		}
		if !preferSlashInTUI(cmd.Name) {
			t.Fatalf("%q should be owned by the richer slash path", name)
		}
	}

	m.input.SetValue("/memory path")
	pressEnter(t, m)
	last := m.messages[len(m.messages)-1].Content
	if !strings.Contains(last, "memory") || strings.Contains(last, "Core Memory") {
		t.Fatalf("/memory args were swallowed by legacy REPL handler: %q", last)
	}

	if preferSlashInTUI("agents") {
		t.Fatal("/agents must remain owned by cmdAgents so it can read the live roster")
	}
	// /memory path opens a modal. Use a fresh model so this assertion tests
	// /agents routing instead of sending Enter to the still-open memory screen.
	agentsModel := newSlashTestModel(t)
	agentsModel.input.SetValue("/agents")
	_, _ = agentsModel.handleSubmit()
	if agentsModel.activeScreen == nil {
		t.Fatal("/agents did not open its roster screen")
	}
	agentsView := agentsModel.activeScreen.View()
	if !strings.Contains(agentsView, "no sub-agents") || strings.Contains(agentsView, "single agent mode") {
		t.Fatalf("/agents did not use the live roster backend: %q", agentsView)
	}
}

func TestRecapReturnsLatestStructuralRecap(t *testing.T) {
	m := newSlashTestModel(t)
	m.messages = append(m.messages,
		Message{Role: "recap", Content: "read 2 files"},
		Message{Role: "info", Content: "later status"},
		Message{Role: "recap", Content: "edited 3 files · tests passed"},
	)
	m.input.SetValue("/recap")
	pressEnter(t, m)
	last := m.messages[len(m.messages)-1].Content
	if !strings.Contains(last, "edited 3 files") || strings.Contains(last, "scroll up") {
		t.Fatalf("/recap did not return latest recap: %q", last)
	}
}

func TestRetryReplacesPriorTurnAndImmediatelyResubmits(t *testing.T) {
	m := newSlashTestModel(t)
	m.loop.Restore([]llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "try this again"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "old response"}}},
	})
	m.messages = append(m.messages,
		Message{Role: "user", Content: "try this again"},
		Message{Role: "assistant", Content: "old response"},
	)
	m.input.SetValue("/retry")
	cmd := pressEnter(t, m)

	if cmd == nil || !m.turnActive {
		t.Fatal("/retry did not immediately start a replacement turn")
	}
	hist := m.loop.History()
	if len(hist) != 1 || hist[0].Role != llm.RoleUser || hist[0].Content[0].Text != "try this again" {
		t.Fatalf("retry history=%+v, want only replacement user prompt before response", hist)
	}
	for _, msg := range m.messages {
		if msg.Role == "assistant" && msg.Content == "old response" {
			t.Fatal("retry retained the old visible assistant response")
		}
	}
}
