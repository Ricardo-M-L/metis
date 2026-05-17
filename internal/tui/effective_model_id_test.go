package tui

// effective_model_id_test.go — pins the chrome's "show the model the
// provider actually sends" contract added 2026-05-17 (user screenshot
// 35: m.model said "deepseek-v4-pro" but the running Anthropic-MiniMax
// provider kept sending "minimax-m2.7"; the banner has to surface the
// wire reality, not the user's intent).

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// newModelWithProvider builds the minimal Model+Loop scaffolding the
// chrome-rendering tests below need: an actual agent.Loop bound to a
// fake Provider, plus a textarea so renderWelcomeBanner doesn't crash.
func newModelWithProvider(t *testing.T, prov llm.Provider, userPickedModel string) *Model {
	t.Helper()
	m := newE2EModel(t, 120, 30, 0)
	m.model = userPickedModel
	m.loop = agent.NewLoop(prov, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	return m
}

// fakeWelcomeProvider is a minimal Provider impl whose ModelID() can
// disagree with whatever string the TUI's m.model holds. Drops every
// other method to the bare minimum needed to satisfy the interface so
// the test focuses on the display path.
type fakeWelcomeProvider struct {
	wireModel string
	maxCtx    int
}

func (p *fakeWelcomeProvider) Name() string          { return "fake-welcome" }
func (p *fakeWelcomeProvider) ModelID() string       { return p.wireModel }
func (p *fakeWelcomeProvider) MaxContextTokens() int { return p.maxCtx }
func (p *fakeWelcomeProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return nil, nil
}
func (p *fakeWelcomeProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	return nil, nil
}

// TestEffectiveModelID_PrefersProviderOverField — when the provider's
// wire model disagrees with m.model, effectiveModelID must return the
// provider's value. This is the diagnostic-truth fix for the screenshot
// 35 scenario.
func TestEffectiveModelID_PrefersProviderOverField(t *testing.T) {
	m := newModelWithProvider(t,
		&fakeWelcomeProvider{wireModel: "minimax-m2.7", maxCtx: 192_000},
		"deepseek-v4-pro" /* what the user picked */)

	got := effectiveModelID(m)
	if got != "minimax-m2.7" {
		t.Errorf("effectiveModelID should prefer Provider.ModelID() over m.model; got %q", got)
	}
}

// TestEffectiveModelID_FallsBackWhenProviderEmpty — providers with an
// empty ModelID() (test fakes, transitional state) must not blank out
// the banner; fall back to m.model.
func TestEffectiveModelID_FallsBackWhenProviderEmpty(t *testing.T) {
	m := newModelWithProvider(t,
		&fakeWelcomeProvider{wireModel: "", maxCtx: 200_000},
		"claude-opus-4-7")

	got := effectiveModelID(m)
	if got != "claude-opus-4-7" {
		t.Errorf("empty Provider.ModelID() should fall back to m.model; got %q", got)
	}
}

// TestRenderWelcomeBanner_UsesProviderWireModel — end-to-end pin: the
// rendered banner string must contain the provider's wire model, not
// the cached m.model, when the two disagree.
func TestRenderWelcomeBanner_UsesProviderWireModel(t *testing.T) {
	m := newModelWithProvider(t,
		&fakeWelcomeProvider{wireModel: "minimax-m2.7", maxCtx: 192_000},
		"deepseek-v4-pro")

	out := m.renderWelcomeBanner()
	stripped := stripANSI(out)
	if !strings.Contains(stripped, "minimax-m2.7") {
		t.Errorf("rendered banner should contain provider's wire model %q; got:\n%s",
			"minimax-m2.7", stripped)
	}
	if strings.Contains(stripped, "deepseek-v4-pro") {
		t.Errorf("rendered banner should NOT contain stale m.model %q when provider disagrees; got:\n%s",
			"deepseek-v4-pro", stripped)
	}
}
