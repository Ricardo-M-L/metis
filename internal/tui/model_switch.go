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
		provider, _, _ := m.loop.ProviderModelSnapshot()
		m.loop.RebindProviderModel(provider, newModel)
		rtpkg.RebindLoopRuntime(m.loop, provider, newModel, m.loop.System, m.sessionID)
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
		provider, _, _ := m.loop.ProviderModelSnapshot()
		m.loop.RebindProviderModel(provider, newModel)
		rtpkg.RebindLoopRuntime(m.loop, provider, newModel, m.loop.System, m.sessionID)
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
	newSystem, newSections := rtpkg.RebindProviderPrompt(
		m.loop.System, m.loop.SystemSections, provName, pb.Model,
	)
	newBaseSystem, newBaseSections := rtpkg.RebindProviderPrompt(
		m.baseSystem, m.baseSystemSections, provName, pb.Model,
	)

	// Swap provider, model, window and compactor math as one loop snapshot.
	m.loop.RebindProviderRuntime(pb.Provider, pb.Model, pb.MaxOutputTokens, newSystem, newSections)
	m.model = pb.Model
	m.providerName = provName
	m.baseSystem = newBaseSystem
	m.baseSystemSections = newBaseSections
	rtpkg.RebindLoopRuntime(m.loop, pb.Provider, pb.Model, newSystem, m.sessionID)

	return nil
}
