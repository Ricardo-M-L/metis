package runtime

// agent_loop_tier_test.go — integration test for SUMMARY #6
// (compaction window tiering). The unit test in
// internal/agent/compact_tier_test.go covered tierForWindow + applyTier
// in isolation; this test pins the actual wiring chain that makes the
// tier visible in production:
//
//   BuildAgentLoop (runtime/agent_loop.go)
//     → NewCompactor                        (uses defaults)
//     → loop.Compactor.MaxOutputTokens = …  (sets max_tokens)
//     → loop.Compactor.ApplyWindowTier(…)   (THIS is the new step)
//     → returned *agent.Loop
//
// Each test starts from a real defaultLoopCfg() and a stubProvider with
// a chosen MaxContextTokens(), runs BuildAgentLoop, and asserts the
// Compactor's runtime-visible fields match the expected tier.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestBuildAgentLoop_TierFor200kWindow(t *testing.T) {
	cfg := defaultLoopCfg(t)
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 200_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
		MaxIter:  10,
	})
	if loop.Compactor == nil {
		t.Fatal("Compactor must be wired")
	}
	if got := loop.Compactor.SnipMaxToolResultChars; got != 3000 {
		t.Errorf("200k window: SnipMaxToolResultChars = %d, want 3000", got)
	}
	if got := loop.Compactor.SnipThreshold; got != 0.80 {
		t.Errorf("200k window: SnipThreshold = %v, want 0.80", got)
	}
}

func TestBuildAgentLoop_TierFor16kWindow(t *testing.T) {
	cfg := defaultLoopCfg(t)
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 16_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
		MaxIter:  10,
	})
	if got := loop.Compactor.SnipMaxToolResultChars; got != 200 {
		t.Errorf("16k window: SnipMaxToolResultChars = %d, want 200", got)
	}
	if got := loop.Compactor.SnipThreshold; got != 0.60 {
		t.Errorf("16k window: SnipThreshold = %v, want 0.60", got)
	}
}

func TestBuildAgentLoop_TierFor64kWindow(t *testing.T) {
	cfg := defaultLoopCfg(t)
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 64_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
		MaxIter:  10,
	})
	// 64k tier values match the historical default (800/0.70) — this
	// is intentional: anything 64k or smaller used to over-snip with
	// a fixed cap; the tier just makes the boundary explicit.
	if got := loop.Compactor.SnipMaxToolResultChars; got != 800 {
		t.Errorf("64k window: SnipMaxToolResultChars = %d, want 800", got)
	}
	if got := loop.Compactor.SnipThreshold; got != 0.70 {
		t.Errorf("64k window: SnipThreshold = %v, want 0.70", got)
	}
}

// TestBuildAgentLoop_TierFor500kWindow — the upper bucket. Modern
// long-context models (Gemini 1.5, Anthropic future) may publish
// >200k windows; we should give them slack.
func TestBuildAgentLoop_TierFor500kWindow(t *testing.T) {
	cfg := defaultLoopCfg(t)
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 500_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
		MaxIter:  10,
	})
	if got := loop.Compactor.SnipMaxToolResultChars; got != 6000 {
		t.Errorf("500k window: SnipMaxToolResultChars = %d, want 6000", got)
	}
	if got := loop.Compactor.SnipThreshold; got != 0.85 {
		t.Errorf("500k window: SnipThreshold = %v, want 0.85", got)
	}
}

// TestBuildAgentLoop_TierMaxOutputTokensEffect — the effective input
// cap is `MaxContextTokens - MaxOutputTokens`. When max_tokens is
// large enough that the effective cap drops a tier, the lookup must
// pick up that drop. Pin so a future "ignore max_tokens" refactor
// can't silently make tiers off-by-one.
//
// Setup: 32k context window, but max_tokens=20k (huge for a small
// model), so the effective input budget is only 12k. tierForWindow
// must return tier-16k (the smallest), not tier-32k.
func TestBuildAgentLoop_TierMaxOutputTokensEffect(t *testing.T) {
	cfg := defaultLoopCfg(t)
	// Force a max_tokens override via the OpenAI compat path. We have
	// to reach into the OpenAI config — defaultLoopCfg leaves it at
	// zero, so the BuildAgentLoop providerMaxTokens fallback returns
	// 0 and the effective cap equals the full window. To isolate the
	// effect we set MaxTokens directly.
	cfg.Provider.OpenAI.MaxTokens = 20_000
	cfg.Provider.Default = "openai"

	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 32_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
		MaxIter:  10,
	})
	// effective cap = 32_000 - 20_000 = 12_000 → tier-16k → 200/0.60.
	if got := loop.Compactor.SnipMaxToolResultChars; got != 200 {
		t.Errorf("eff-cap 12k: SnipMaxToolResultChars = %d, want 200 (tier-16k after max_tokens drop)", got)
	}
}

func TestBuildAgentLoop_OpenAICodexReservesConfiguredOutputBudget(t *testing.T) {
	cfg := defaultLoopCfg(t)
	cfg.Provider.Default = "openai-codex"
	cfg.Provider.OpenAICodex.MaxTokens = 16_000
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider: &stubProvider{maxCtx: 200_000},
		Registry: tools.NewRegistry(),
		Gate:     permission.New(permission.ModeAcceptEdits),
		MaxIter:  10,
	})
	if got := loop.Compactor.MaxOutputTokens; got != 16_000 {
		t.Fatalf("Codex MaxOutputTokens = %d, want 16000", got)
	}
}

func TestBuildAgentLoop_ResolvedOutputBudgetOverridesConfigDefault(t *testing.T) {
	cfg := defaultLoopCfg(t)
	cfg.Provider.Default = "anthropic"
	cfg.Provider.Anthropic.MaxTokens = 64_000
	loop := BuildAgentLoop(cfg, AgentLoopOptions{
		Provider:        &stubProvider{maxCtx: 32_000},
		MaxOutputTokens: 20_000,
		Registry:        tools.NewRegistry(),
		Gate:            permission.New(permission.ModeAcceptEdits),
		MaxIter:         10,
	})
	if got := loop.Compactor.MaxOutputTokens; got != 20_000 {
		t.Fatalf("resolved MaxOutputTokens = %d, want 20000", got)
	}
	if got := loop.Compactor.SnipMaxToolResultChars; got != 200 {
		t.Fatalf("resolved effective cap tier = %d chars, want 200", got)
	}
}
