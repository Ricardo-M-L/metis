package builtin

// agent_g9_test.go — locks Phase G.9 (2026-05-12) per-invocation
// permission_mode override contract.
//
// Three contracts:
//
//   1. Default path (no permission_mode arg) — sub-agent inherits the
//      parent's mode via the clone.
//   2. `permission_mode: "bypass"` arg — sub-agent's gate is in
//      bypass even when parent is in ask.
//   3. Override doesn't leak back to the parent gate.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// TestAgentExecute_DefaultModeInherits — without permission_mode, the
// sub-agent's gate mirrors the parent's mode. Probed via the
// transcript header that captures mode at sub-loop construction.
func TestAgentExecute_DefaultModeInherits(t *testing.T) {
	dir := t.TempDir()
	gate := permission.New(permission.ModeAsk)
	roster := agent.NewRoster(0)
	tool := NewAgent(gate, helloProvider(), tools.NewRegistry(), "m", "s").
		WithRoster(roster).
		WithSessionPersistence(dir, "parent")

	res, err := tool.Execute(context.Background(), map[string]any{"prompt": "x"})
	if err != nil || res.IsError {
		t.Fatalf("Execute: err=%v res=%+v", err, res)
	}
	agentID := mustOneTranscript(t, dir)
	snap, err := agent.LoadSubAgentSnapshot(dir, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Header.Mode != "ask" {
		t.Errorf("Default sub-agent should inherit parent mode 'ask'; got %q", snap.Header.Mode)
	}
}

// TestAgentExecute_PermissionModeOverridesChild — passing
// permission_mode="bypass" flips ONLY the sub-agent's gate; the
// parent stays in ask.
func TestAgentExecute_PermissionModeOverridesChild(t *testing.T) {
	dir := t.TempDir()
	gate := permission.New(permission.ModeAsk)
	roster := agent.NewRoster(0)
	tool := NewAgent(gate, helloProvider(), tools.NewRegistry(), "m", "s").
		WithRoster(roster).
		WithSessionPersistence(dir, "parent")

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":          "x",
		"permission_mode": "bypass",
	})
	if err != nil || res.IsError {
		t.Fatalf("Execute: err=%v res=%+v", err, res)
	}
	agentID := mustOneTranscript(t, dir)
	snap, err := agent.LoadSubAgentSnapshot(dir, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Header.Mode != "bypass" {
		t.Errorf("override sub-agent mode = %q, want bypass", snap.Header.Mode)
	}
	// Parent gate unchanged.
	if gate.Mode() != permission.ModeAsk {
		t.Errorf("parent gate leaked override: now %q", gate.Mode())
	}
}

// mustOneTranscript reads the SubAgentTranscriptDirname under dir and
// returns the basename (without .jsonl) of the single transcript
// expected. Fails the test loudly when 0 or >1 exist.
func mustOneTranscript(t *testing.T, dir string) string {
	t.Helper()
	subDir := filepath.Join(dir, agent.SubAgentTranscriptDirname)
	entries, err := os.ReadDir(subDir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", subDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 transcript under %s, got %d (%v)", subDir, len(entries), entries)
	}
	return strings.TrimSuffix(entries[0].Name(), ".jsonl")
}
