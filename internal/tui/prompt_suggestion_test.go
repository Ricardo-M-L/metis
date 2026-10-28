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
