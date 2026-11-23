package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

type promptSuggestionProvider struct {
	calls    int
	complete func(context.Context, provider.Request) (*provider.Response, error)
}

func (*promptSuggestionProvider) Name() string          { return "suggestion-fixture" }
func (*promptSuggestionProvider) ModelID() string       { return "suggestion-model" }
func (*promptSuggestionProvider) MaxContextTokens() int { return 128000 }
func (p *promptSuggestionProvider) Complete(ctx context.Context, req provider.Request) (*provider.Response, error) {
	p.calls++
	if p.complete != nil {
		return p.complete(ctx, req)
	}
	return &provider.Response{StopReason: "end_turn", Content: []provider.ContentBlock{{Type: "text", Text: "给这个改动补测试"}}}, nil
}
func (*promptSuggestionProvider) Stream(context.Context, provider.Request) (provider.StreamReader, error) {
	return nil, errors.New("accepting ghost text must not submit a turn")
}

func newPromptSuggestionModel(t *testing.T) (*Model, *promptSuggestionProvider) {
	t.Helper()
	p := &promptSuggestionProvider{}
	m := newE2EModel(t, 80, 30, 0)
	m.ctx = context.Background()
	m.cfg = &config.Config{UI: config.UI{PromptSuggestions: true}}
	m.sessionID, m.model, m.providerName = "suggestion-session", "suggestion-model", "suggestion-fixture"
	m.input.Placeholder = defaultInputPlaceholder
	m.loop = agent.NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "system", 4)
	m.loop.Restore([]provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "修复这个问题"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "text", Text: "问题已修复"}}},
	})
	return m, p
}

func preparePromptSuggestion(t *testing.T, m *Model) tea.Cmd {
	t.Helper()
	m.queuePromptSuggestion()
	cmd := m.startPromptSuggestion()
	if cmd == nil {
		t.Fatal("eligible completed conversation did not schedule a suggestion")
	}
	if duplicate := m.startPromptSuggestion(); duplicate != nil {
		t.Fatal("suggestion request was scheduled twice")
	}
	return cmd
}

func TestPromptSuggestionIsGhostUntilTabAndNeverAutoSends(t *testing.T) {
	m, p := newPromptSuggestionModel(t)
	cmd := preparePromptSuggestion(t, m)
	m.Update(cmd())
	if p.calls != 1 || m.input.Value() != "" || m.input.Placeholder != "给这个改动补测试" {
		t.Fatalf("ghost state: calls=%d value=%q placeholder=%q", p.calls, m.input.Value(), m.input.Placeholder)
	}
	if got := stripANSI(renderInputLine(m)); !strings.Contains(got, "给这个改动补测试") {
		t.Fatalf("suggestion is not visible in the input: %q", got)
	}
	before := len(m.loop.History())
	_, command := m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	if command != nil || m.turnActive || len(m.loop.History()) != before || p.calls != 1 {
		t.Fatal("Tab submitted the suggestion instead of just accepting it")
	}
	if m.input.Value() != "给这个改动补测试" || m.input.Placeholder != defaultInputPlaceholder {
		t.Fatalf("accepted draft=%q placeholder=%q", m.input.Value(), m.input.Placeholder)
	}
}

func TestPromptSuggestionAcceptedDraftCanBeErasedAndAcceptedAgain(t *testing.T) {
	m, p := newPromptSuggestionModel(t)
	m.input.Focus()
	m.Update(preparePromptSuggestion(t, m)())
	const suggestion = "给这个改动补测试"
	before := len(m.loop.History())
	for attempt := 0; attempt < 3; attempt++ {
		m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		if m.input.Value() != suggestion {
			t.Fatalf("accept %d: draft=%q", attempt, m.input.Value())
		}
		// Exercise ordinary editor updates, including partial edits, rather
		// than directly resetting the textarea or the suggestion state.
		m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
		if attempt == 1 {
			m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
			if m.input.Value() != "" {
				t.Fatal("Ctrl+U did not clear the accepted draft")
			}
		}
		for m.input.Value() != "" {
			previous := m.input.Value()
			m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
			if m.input.Value() == previous {
				t.Fatal("Backspace did not edit the accepted draft")
			}
		}
		if m.input.Placeholder != suggestion || !strings.Contains(stripANSI(renderInputLine(m)), suggestion) {
			t.Fatalf("erase %d: cached ghost did not reappear: %q", attempt, m.input.Placeholder)
		}
		if p.calls != 1 || m.turnActive || len(m.loop.History()) != before || m.startPromptSuggestion() != nil {
			t.Fatal("restoring an accepted suggestion made a model request or sent a turn")
		}
	}
}

func TestPromptSuggestionAcceptedDraftSurvivesPasteThenErase(t *testing.T) {
	m, p := newPromptSuggestionModel(t)
	m.input.Focus()
	m.Update(preparePromptSuggestion(t, m)())
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m.Update(tea.PasteMsg{Content: "补上边界情况"})
	if !strings.Contains(m.input.Value(), "补上边界情况") {
		t.Fatal("paste did not reach the accepted draft")
	}
	for m.input.Value() != "" {
		m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	if m.input.Placeholder != "给这个改动补测试" || p.calls != 1 {
		t.Fatal("pasting into an accepted draft discarded its cached suggestion")
	}
}

func TestPromptSuggestionDirectSubmitExpiresAcceptedDraft(t *testing.T) {
	m, p := newPromptSuggestionModel(t)
	m.Update(preparePromptSuggestion(t, m)())
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m.input.SetValue("/later verify")
	m.turnActive = true
	// Exercise the submission entry point without a preceding Enter key.
	// Explicit queueing consumes the draft without running a model here.
	m.handleSubmit()
	if m.input.Value() != "" || len(m.queuedPrompts) != 1 {
		t.Fatal("submission did not consume and queue the draft")
	}
	m.turnActive = false
	m.queuedPrompts = nil
	m.reconcilePromptSuggestion()
	if m.input.Placeholder != defaultInputPlaceholder || m.acceptPromptSuggestion() || p.calls != 1 {
		t.Fatal("submitted suggestion reappeared after the queue cleared")
	}
}

func TestPromptSuggestionAcceptedDraftDoesNotRestoreAfterExpiry(t *testing.T) {
	for name, expire := range map[string]func(*Model){
		"escape":   func(m *Model) { m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}) },
		"session":  func(m *Model) { m.sessionID = "other-session"; m.reconcilePromptSuggestion() },
		"model":    func(m *Model) { m.model = "other-model"; m.reconcilePromptSuggestion() },
		"provider": func(m *Model) { m.providerName = "other-provider"; m.reconcilePromptSuggestion() },
		"new turn": func(m *Model) { m.turnActive = true; m.reconcilePromptSuggestion(); m.turnActive = false },
		"disabled": func(m *Model) {
			m.cfg.UI.PromptSuggestions = false
			m.reconcilePromptSuggestion()
			m.cfg.UI.PromptSuggestions = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, p := newPromptSuggestionModel(t)
			m.Update(preparePromptSuggestion(t, m)())
			m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			expire(m)
			m.input.Reset()
			m.reconcilePromptSuggestion()
			if m.input.Placeholder != defaultInputPlaceholder || m.acceptPromptSuggestion() || p.calls != 1 {
				t.Fatal("expired accepted suggestion reappeared or requested another suggestion")
			}
		})
	}
}

func TestPromptSuggestionDoesNotOverwriteDraftsOrStealCompletion(t *testing.T) {
	for _, value := range []string{"my draft", "  ", "/he", "@file"} {
		t.Run(value, func(t *testing.T) {
			m, p := newPromptSuggestionModel(t)
			m.input.SetValue(value)
			m.queuePromptSuggestion()
			if cmd := m.startPromptSuggestion(); cmd != nil || p.calls != 0 {
				t.Fatal("suggestion started while a draft/completion existed")
			}
			if m.acceptPromptSuggestion() || m.input.Value() != value {
				t.Fatal("suggestion stole the existing draft")
			}
		})
	}
}

func TestPromptSuggestionSkipsBusyDisabledAndInteractiveStates(t *testing.T) {
	for name, setup := range map[string]func(*Model){
		"disabled":   func(m *Model) { m.cfg.UI.PromptSuggestions = false },
		"running":    func(m *Model) { m.turnActive = true },
		"shell mode": func(m *Model) { m.shellMode = true },
		"copy mode":  func(m *Model) { m.copyMode = true },
		"spinner":    func(m *Model) { m.spinnerActive = true },
		"queue":      func(m *Model) { m.queuePending = true },
		"permission": func(m *Model) { m.permActive = true },
		"question":   func(m *Model) { m.askUserActive = true },
		"palette":    func(m *Model) { m.showPalette = true },
		"attachment": func(m *Model) { m.imagePaste = map[int]string{1: "image.png"} },
	} {
		t.Run(name, func(t *testing.T) {
			m, p := newPromptSuggestionModel(t)
			setup(m)
			m.queuePromptSuggestion()
			if cmd := m.startPromptSuggestion(); cmd != nil || p.calls != 0 {
				t.Fatal("ineligible state scheduled a model request")
			}
		})
	}
}

func TestPromptSuggestionRejectsLateResultsAfterOwnershipOrInputChanges(t *testing.T) {
	for name, change := range map[string]func(*Model){
		"session": func(m *Model) { m.sessionID = "other-session" },
		"model":   func(m *Model) { m.model = "other-model" },
		"disabled": func(m *Model) {
			m.cfg.UI.PromptSuggestions = false
		},
		"typed then erased": func(m *Model) {
			m.handleKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
			m.input.Reset()
		},
		"escape": func(m *Model) { m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape}) },
		"paste":  func(m *Model) { m.Update(tea.PasteMsg{Content: "draft"}) },
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := newPromptSuggestionModel(t)
			cmd := preparePromptSuggestion(t, m)
			result := cmd()
			change(m)
			m.Update(result)
			if m.input.Placeholder != defaultInputPlaceholder || m.acceptPromptSuggestion() {
				t.Fatal("late suggestion reappeared after ownership/input changed")
			}
		})
	}
}

func TestPromptSuggestionFinalizationOnlySchedulesSuccessfulTurn(t *testing.T) {
	for _, err := range []error{nil, context.Canceled, errors.New("request failed"), &agent.IncompleteRunError{Reason: "max_tokens"}} {
		m, _ := newPromptSuggestionModel(t)
		m.finalizeTurn(err)
		cmd := m.startPromptSuggestion()
		if (cmd != nil) != (err == nil) {
			t.Fatalf("suggestion scheduled=%v after completion error=%v", cmd != nil, err)
		}
		m.clearPromptSuggestion()
	}
}

func TestPromptSuggestionLiveDisableClearsGhost(t *testing.T) {
	m, _ := newPromptSuggestionModel(t)
	m.Update(preparePromptSuggestion(t, m)())
	candidate := &config.Config{UI: config.UI{PromptSuggestions: false}}
	if applied := m.applyLiveConfigChanges(candidate, []screen.ConfigChange{{Key: "ui.prompt_suggestions", Value: "false"}}); applied != 1 {
		t.Fatalf("live settings applied=%d, want 1", applied)
	}
	if m.input.Value() != "" || m.input.Placeholder != defaultInputPlaceholder || m.acceptPromptSuggestion() {
		t.Fatal("disabling suggestions retained the ghost or changed the draft")
	}
}
