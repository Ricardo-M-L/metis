package tui

// model_switch_test.go — pins the mid-session /model rebuild path
// added 2026-05-17 (user screenshot 35 desync: switching the model
// string left the underlying Provider unchanged so cap + transport +
// auth all stayed on the previous profile). Covers:
//
//   1. Graceful degrade when cfg is nil / profile missing — the test
//      surfaces don't crash, string fields still update.
//   2. Full rebuild when cfg has the target profile — Provider swaps,
//      Compactor refreshes, m.model + m.loop.Model + m.loop.Provider
//      all reflect the new selection.

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// switchTestProvider keeps things minimal: just enough to satisfy
// llm.Provider so we can swap one for another and read ModelID() /
// MaxContextTokens() back. NOT exported because two test files
// shouldn't share the name (we already have fakeWelcomeProvider).
type switchTestProvider struct {
	id     string
	maxCtx int
}

func (p *switchTestProvider) Name() string          { return "switch-test" }
func (p *switchTestProvider) ModelID() string       { return p.id }
func (p *switchTestProvider) MaxContextTokens() int { return p.maxCtx }
func (p *switchTestProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return nil, nil
}
func (p *switchTestProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	return nil, nil
}

// TestSwitchModel_NilCfgStillUpdatesStrings — no real cfg wired
// (legacy test scaffolding) must NOT block the string-side update.
// Otherwise every test that calls /model would crash.
func TestSwitchModel_NilCfgStillUpdatesStrings(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.loop = agent.NewLoop(&switchTestProvider{id: "old", maxCtx: 100_000},
		tools.NewRegistry(),
		permission.New(permission.ModeAcceptEdits),
		nil, "sys", 5)
	m.cfg = nil // crucial: no config

	err := m.switchModel("new-model", "")
	if err != nil {
		t.Fatalf("nil cfg path should not return error; got %v", err)
	}
	if m.model != "new-model" {
		t.Errorf("m.model should still update; got %q", m.model)
	}
	if m.loop.Model != "new-model" {
		t.Errorf("loop.Model should still update; got %q", m.loop.Model)
	}
	// Provider must NOT have been swapped — there's no profile to
	// rebuild against.
	if got := m.loop.Provider.ModelID(); got != "old" {
		t.Errorf("Provider should stay on the old impl (no rebuild); got ModelID() %q", got)
	}
}

// TestSwitchModel_RebuildsProviderForKnownProfile — when cfg has the
// target profile configured, the full rebuild path runs: a NEW Provider
// gets swapped in, loop.ContextWindow refreshes, m.providerName tracks
// the new profile, Compactor's MaxContextTokens updates so ShouldCompact
// uses the new cap. Uses the openai built-in (no API key required for
// New() — failure mode would be a Stream() call, which the test doesn't
// make).
func TestSwitchModel_RebuildsProviderForKnownProfile(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	oldProv := &switchTestProvider{id: "old-model", maxCtx: 50_000}
	m.loop = agent.NewLoop(oldProv,
		tools.NewRegistry(),
		permission.New(permission.ModeAcceptEdits),
		nil, "sys", 5)
	m.loop.Compactor = agent.NewCompactor(agent.DefaultCompactionConfig(), "old-model", 50_000, oldProv)
	m.providerName = "openai"
	m.cfg = &config.Config{}
	// Minimal openai profile so BuildProvider succeeds. APIKey can be a
	// placeholder — Stream() isn't called by switchModel.
	m.cfg.Provider.OpenAI.APIKey = "test-openai-key"
	m.cfg.Provider.OpenAI.Model = "gpt-4o-mini"

	err := m.switchModel("gpt-4o-mini", "openai")
	if err != nil {
		t.Fatalf("rebuild should succeed; got %v", err)
	}
	if m.providerName != "openai" {
		t.Errorf("providerName should be 'openai'; got %q", m.providerName)
	}
	if m.model != "gpt-4o-mini" {
		t.Errorf("m.model should be 'gpt-4o-mini'; got %q", m.model)
	}
	if m.loop.Provider == oldProv {
		t.Errorf("Provider should have been swapped; old Provider still in place")
	}
	if got := m.loop.Provider.ModelID(); got != "gpt-4o-mini" {
		t.Errorf("Provider.ModelID() should reflect new model; got %q", got)
	}
	// Compactor's cap should track the new Provider's MaxContextTokens.
	if m.loop.Compactor == nil {
		t.Fatal("Compactor should still exist after rebuild")
	}
	if m.loop.Compactor.MaxContextTokens != m.loop.Provider.MaxContextTokens() {
		t.Errorf("Compactor.MaxContextTokens (%d) should match Provider.MaxContextTokens (%d) after rebuild",
			m.loop.Compactor.MaxContextTokens, m.loop.Provider.MaxContextTokens())
	}
}

// TestSwitchModel_UnknownProfileReturnsErrorButStringsUpdate — when
// the new profile isn't in cfg, BuildProvider errors; the call returns
// that error so callers can surface a warning, but the string fields
// still update so the chrome reflects the user's intent.
func TestSwitchModel_UnknownProfileReturnsErrorButStringsUpdate(t *testing.T) {
	m := newE2EModel(t, 120, 30, 0)
	m.loop = agent.NewLoop(&switchTestProvider{id: "old", maxCtx: 100_000},
		tools.NewRegistry(),
		permission.New(permission.ModeAcceptEdits),
		nil, "sys", 5)
	m.cfg = &config.Config{} // no providers configured

	err := m.switchModel("new-model", "nonexistent-profile")
	if err == nil {
		t.Error("rebuild should return error for unknown profile")
	}
	if m.model != "new-model" {
		t.Errorf("strings should still update on rebuild failure; m.model=%q", m.model)
	}
}
