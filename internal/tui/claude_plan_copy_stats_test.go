package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	runtimepkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestCopyNumericArgumentSelectsNthLatestAssistantReply(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	loop := agent.NewLoop(fakeProvider{}, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "sys", 2)
	loop.Restore([]llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "oldest"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "next"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "second newest"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "newest"}}},
	})

	out := cmdCopy(&REPL{Loop: loop}, "2")
	body, err := os.ReadFile(filepath.Join(os.Getenv("METIS_HOME"), "clipboard.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "second newest" {
		t.Fatalf("/copy 2 copied %q, want the second-latest assistant reply", got)
	}
	if !strings.Contains(out, "second-latest") {
		t.Fatalf("confirmation does not identify the selected reply: %q", out)
	}
}

func TestCopyBareStillSelectsLatestAssistantReply(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	loop := agent.NewLoop(fakeProvider{}, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "sys", 2)
	loop.Restore([]llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "older"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "latest"}}},
	})

	cmdCopy(&REPL{Loop: loop}, "")
	body, err := os.ReadFile(filepath.Join(os.Getenv("METIS_HOME"), "clipboard.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "latest" {
		t.Fatalf("bare /copy copied %q, want latest assistant reply", got)
	}
}

func TestPlanBareEntersModeThenShowsCurrentPlan(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newSlashTestModel(t)
	m.sessionID = "plan-session"

	m.input.SetValue("/plan")
	pressEnter(t, m)
	if got := m.gate.Mode(); got != permission.ModePlan {
		t.Fatalf("first /plan gate mode = %q, want plan", got)
	}
	if !m.loop.IsPlanMode() {
		t.Fatal("first /plan did not activate the agent loop's plan mode")
	}

	const body = "# Current plan\n\n1. Inspect the parser.\n2. Add coverage."
	if err := runtimepkg.WriteCurrentPlan(m.sessionID, body); err != nil {
		t.Fatal(err)
	}
	before := len(m.messages)
	m.input.SetValue("/plan")
	pressEnter(t, m)
	if len(m.messages) <= before {
		t.Fatal("bare /plan while already planning produced no current-plan output")
	}
	got := m.messages[len(m.messages)-1].Content
	if !strings.Contains(got, "Current Plan") || !strings.Contains(got, "Inspect the parser") {
		t.Fatalf("bare /plan did not show the current plan:\n%s", got)
	}
}

func TestPlanDescriptionUpdatesDraftAndStartsPlanningTurn(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newSlashTestModelWithLoop(t)
	m.sessionID = "plan-description-session"
	const description = "replace the legacy parser without changing public flags"

	m.input.SetValue("/plan " + description)
	cmd := pressEnter(t, m)
	if cmd == nil {
		t.Fatal("/plan <description> did not start an agent turn")
	}
	if m.gate.Mode() != permission.ModePlan || !m.loop.IsPlanMode() {
		t.Fatalf("plan description did not enter plan mode: gate=%q loop=%v", m.gate.Mode(), m.loop.IsPlanMode())
	}
	history := m.loop.History()
	if len(history) == 0 || history[0].Role != llm.RoleUser || !strings.Contains(history[0].Content[0].Text, description) {
		t.Fatalf("plan description did not reach loop history: %+v", history)
	}
	draft, err := runtimepkg.ReadCurrentPlan(m.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(draft, description) {
		t.Fatalf("current plan draft missing description: %q", draft)
	}
	if m.turnCancel != nil {
		m.turnCancel()
	}
}

func TestPlanOpenUsesManagedEditorAndCreatesSessionDraft(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newSlashTestModel(t)
	m.sessionID = "plan-open-session"
	m.input.SetValue("/plan open")

	cmd := pressEnter(t, m)
	if cmd == nil {
		t.Fatal("/plan open must return a Bubble Tea managed editor command")
	}
	path := runtimepkg.CurrentPlanPath(m.sessionID)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("/plan open did not create current plan at %s: %v", path, err)
	}
	if m.gate.Mode() != permission.ModePlan || !m.loop.IsPlanMode() {
		t.Fatalf("/plan open did not enter plan mode: gate=%q loop=%v", m.gate.Mode(), m.loop.IsPlanMode())
	}
}

func TestPlainREPLPlanDescriptionAndRepeatedBarePlan(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	reg := slash.NewRegistry()
	slash.RegisterAll(reg, &config.Config{})
	r, out := newPromptTestREPL("/plan inspect the cache boundary\n/plan\n/quit\n", reg)
	r.SessionID = "plain-plan-session"

	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Gate.Mode() != permission.ModePlan || !r.Loop.IsPlanMode() {
		t.Fatalf("plain REPL did not remain in plan mode: gate=%q loop=%v", r.Gate.Mode(), r.Loop.IsPlanMode())
	}
	history := r.Loop.History()
	if len(history) == 0 || !strings.Contains(history[0].Content[0].Text, "inspect the cache boundary") {
		t.Fatalf("plain REPL plan description did not reach history: %+v", history)
	}
	stdout := stripANSI(out.String())
	if !strings.Contains(stdout, "mode set: plan") || !strings.Contains(stdout, "Current Plan") || !strings.Contains(stdout, "inspect the cache boundary") {
		t.Fatalf("plain REPL plan lifecycle output incomplete:\n%s", stdout)
	}
}

func TestPlainREPLPlanOpenEditsStableDraft(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("editor fixture uses a POSIX script")
	}
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	editor := filepath.Join(t.TempDir(), "editor")
	script := "#!/bin/sh\nprintf '\\nEdited by test\\n' >> \"$1\"\n"
	if err := os.WriteFile(editor, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", editor)
	t.Setenv("EDITOR", "")
	reg := slash.NewRegistry()
	slash.RegisterAll(reg, &config.Config{})
	r, _ := newPromptTestREPL("/plan open\n/quit\n", reg)
	r.SessionID = "plain-open-session"

	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	body, err := runtimepkg.ReadCurrentPlan(r.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "Edited by test") {
		t.Fatalf("plain /plan open did not edit the stable draft: %q", body)
	}
}

func TestExitPlanProposalBecomesCurrentEditablePlan(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := newSlashTestModel(t)
	m.sessionID = "proposal-session"
	m.handleAgentEvent(agent.Event{Kind: agent.EventInfo, Info: "[plan proposal]\n# Final proposal\n\n1. Ship it."})

	body, err := runtimepkg.ReadCurrentPlan(m.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "Final proposal") || strings.Contains(body, "[plan proposal]") {
		t.Fatalf("proposal was not persisted as clean current plan markdown: %q", body)
	}
}

func TestExitPlanProposalReportsPersistenceFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	// WriteCurrentPlan needs $METIS_HOME/plans to be a directory. A regular
	// file gives us a deterministic failure without platform-specific modes.
	if err := os.WriteFile(filepath.Join(home, "plans"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newSlashTestModel(t)
	m.sessionID = "proposal-write-failure"
	m.handleAgentEvent(agent.Event{Kind: agent.EventInfo, Info: "[plan proposal]\n# Proposal that cannot be saved"})

	if len(m.messages) < 2 {
		t.Fatalf("expected proposal plus persistence error, got %#v", m.messages)
	}
	if got := m.messages[len(m.messages)-1]; got.Role != "error" || !strings.Contains(got.Content, "failed to save current plan") {
		t.Fatalf("missing truthful plan persistence error: %#v", got)
	}
}

func TestStatsIncludesCrossSessionUsageActivityAndCurrentSessionSection(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	store, err := session.NewStore(filepath.Join(os.Getenv("METIS_HOME"), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		id, model string
		in, out   int
	}{
		{id: "current", model: "model-a", in: 1000, out: 100},
		{id: "other", model: "model-b", in: 300, out: 30},
	} {
		if err := store.WriteHeader(fixture.id, fixture.model, "system"); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage(fixture.id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}); err != nil {
			t.Fatal(err)
		}
		if err := store.WriteCost(fixture.id, session.CostSnapshot{InputTokens: fixture.in, OutputTokens: fixture.out}); err != nil {
			t.Fatal(err)
		}
	}

	m := newSlashTestModel(t)
	m.session = store
	m.sessionID = "current"
	m.messages = append(m.messages, Message{Role: "user", Content: "current prompt"})
	m.totalTokens.add(1000, 100, 0, 0)
	out := stripANSI(renderStats(m))
	for _, want := range []string{
		"Usage & Activity",
		"All Sessions",
		"sessions:",
		"2",
		"active days:",
		"input tokens:",
		"1,300",
		"30-day activity:",
		"Current Session Stats",
		"current",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("cross-session /stats missing %q:\n%s", want, out)
		}
	}
}
