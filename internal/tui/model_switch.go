package tui

// model_switch.go — mid-session /model switch handler. Before this
// file existed, /model only updated m.model + loop.Model strings; the
// underlying llm.Provider (with its baked-in ContextWindow, MaxTokens,
// base URL, transport, etc.) kept running unchanged. Result: the
// status bar said "deepseek-v4-pro" while requests still went to the
// MiniMax-Anthropic gateway (user screenshot 35, 2026-05-17).
//
// switchModel rebuilds the Provider from m.cfg every time the user
// changes models, so the new selection takes real effect: requests
// route to the right backend, the auto-compactor refreshes its
// MaxContextTokens, and the chrome banner reflects the truth.

import (
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/agent"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

// switchModel rebuilds m.loop.Provider for the (newModel, newProvName)
// pair. Empty newProvName means "stay on the current provider profile,
// just change the model" — useful for /model <id> inline where the
// user typed only a model name and the caller has no profile hint.
//
// On success: m.model / m.loop.Model / m.loop.Provider / m.loop.ContextWindow
// and m.loop.Compactor's MaxContextTokens all reflect the new selection,
// AND m.providerName tracks the active profile for the next switch.
//
// On error (missing API key for the new profile, unknown profile, etc.):
// nothing changes — the live Provider and Compactor stay valid. The
// error string is suitable for surfacing in an info row.
func (m *Model) switchModel(newModel, newProvName string) error {
	if m == nil || m.loop == nil {
		return fmt.Errorf("model switch: TUI not fully wired (loop missing)")
	}
	if newModel == "" {
		return fmt.Errorf("model switch: empty model id")
	}
	if m.turnActive {
		return fmt.Errorf("model switch: running turn is active")
	}
	if m.rewindSummaryPending {
		return fmt.Errorf("model switch: rewind summary is active")
	}

	if m.cfg == nil {
		m.model = newModel
		m.loop.Model = newModel
		rtpkg.RebindLoopRuntime(m.loop, m.loop.Provider, newModel, m.loop.System, m.sessionID)
		return nil // string-only swap; cfg not wired (test path)
	}

	provName := newProvName
	if provName == "" {
		provName = m.providerName
	}
	if provName == "" {
		provName = m.cfg.Provider.Default
	}
	if provName == "" {
		m.model = newModel
		m.loop.Model = newModel
		rtpkg.RebindLoopRuntime(m.loop, m.loop.Provider, newModel, m.loop.System, m.sessionID)
		return nil // no provider profile to rebuild against
	}

	pb, err := rtpkg.BuildProvider(m.cfg, provName, newModel)
	if err != nil {
		// Atomic failure: keep the old model strings and Provider together.
		// A chrome label that disagrees with the transport is more dangerous
		// than leaving the prior selection visible.
		return fmt.Errorf("model switch: BuildProvider(%s, %s): %w",
			provName, newModel, err)
	}

	// Swap in the new Provider. From this point on the loop's next
	// request uses the new transport + auth + window.
	m.loop.Provider = pb.Provider
	m.loop.Model = pb.Model
	m.loop.ContextWindow = pb.Provider.MaxContextTokens()
	m.model = pb.Model
	m.providerName = provName
	rtpkg.RebindLoopRuntime(m.loop, pb.Provider, pb.Model, m.loop.System, m.sessionID)

	// Rebuild Compactor so ShouldCompact / threshold math uses the
	// new provider's MaxContextTokens. Preserve the existing Config
	// (Threshold, MinimumTokens, MicrocompactDir, IdleMaxSeconds, …)
	// so the user's session-level overrides survive the swap; the only
	// inputs that change are the per-provider cap and the per-provider
	// per-request output budget.
	if m.loop.Compactor != nil {
		oldCfg := m.loop.Compactor.Config
		oldMaxOut := m.loop.Compactor.MaxOutputTokens
		m.loop.Compactor = agent.NewCompactor(oldCfg, pb.Model,
			pb.Provider.MaxContextTokens(), pb.Provider)
		// Carry max_tokens forward from before. providerMaxTokens(cfg)
		// reads cfg.Provider.Default which may not match the new
		// profile — so we use the previous value as a conservative
		// approximation. Mismatches cause the trigger to fire slightly
		// early/late but never produce a wire-cap violation.
		m.loop.Compactor.MaxOutputTokens = oldMaxOut
		m.loop.Compactor.ApplyWindowTier(
			pb.Provider.MaxContextTokens() - oldMaxOut,
		)
	}
	return nil
}
