package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestContextCompactedDropsPreCompactUsage reproduces the user's 500k →
// compact → still-500k status bar. The provider counters belong to the request
// made before compaction; after a successful history replacement, the local
// post-compact estimate must become the status-bar source immediately.
func TestContextCompactedDropsPreCompactUsage(t *testing.T) {
	m := minimalModel(262_144)
	m.totalTokens.add(500_393, 11_000, 0, 0)
	m.loop.Restore([]llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Type: "text",
			Text: strings.Repeat("x", 120_000),
		}},
	}})
	postTokens := m.loop.EstimateContextTokens()
	if postTokens <= 0 || postTokens >= 500_393 {
		t.Fatalf("bad test fixture postTokens=%d", postTokens)
	}

	m.handleAgentEvent(agent.Event{
		Kind:                  agent.EventContextCompacted,
		Info:                  "compact",
		PreviousContextTokens: 500_393,
		ContextTokens:         postTokens,
	})

	if got := m.totalTokens.ContextUsage(); got != 0 {
		t.Fatalf("pre-compact ContextUsage survived success: got %d", got)
	}
	if m.totalTokens.Input() != 0 || m.totalTokens.Output() != 0 {
		t.Fatalf("/cost counters must restart at compact boundary: in=%d out=%d",
			m.totalTokens.Input(), m.totalTokens.Output())
	}

	bar := stripANSI(renderStatusBar(m))
	if strings.Contains(bar, "500393 tokens") {
		t.Fatalf("status bar retained pre-compact usage:\n%s", bar)
	}
	if !strings.Contains(bar, formatTokensRaw(postTokens)+" tokens") {
		t.Fatalf("status bar missing post-compact estimate %d:\n%s", postTokens, bar)
	}
	if strings.Contains(bar, "99%+") || strings.Contains(bar, "(100%)") {
		t.Fatalf("small post-compact history rendered as full:\n%s", bar)
	}
}

// TestCompactionEndFailureOnlyClearsProgress verifies compact_end semantics:
// a failed attempt releases the animation but cannot reset token state or
// claim that a smaller context was installed.
func TestCompactionEndFailureOnlyClearsProgress(t *testing.T) {
	m := minimalModel(262_144)
	m.totalTokens.add(500_393, 0, 0, 0)
	m.spinnerOverride = "Compacting conversation..."
	m.spinnerCompactionBytes = 12_345

	m.handleAgentEvent(agent.Event{
		Kind: agent.EventCompactionEnd,
		Info: "compact",
		Err:  errors.New("summary unavailable"),
	})

	if m.spinnerOverride != "" || m.spinnerCompactionBytes != 0 {
		t.Fatalf("failed attempt did not clear progress state: override=%q bytes=%d",
			m.spinnerOverride, m.spinnerCompactionBytes)
	}
	if got := m.totalTokens.ContextUsage(); got != 500_393 {
		t.Fatalf("failed attempt reset usage: got %d, want 500393", got)
	}
}
