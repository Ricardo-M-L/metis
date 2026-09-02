package builtin

// agent_g9_test.go — locks Phase G.9 (2026-05-12) per-invocation
// permission_mode override contract.
//
// Three contracts:
//
//   1. Default path (no permission_mode arg) — sub-agent inherits the
//      parent's mode via the clone.
//   2. A child cannot elevate a default parent to bypassPermissions.
//   3. A bypass parent is inherited without leaking mode changes back.

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
	if snap.Header.Mode != "default" {
		t.Errorf("Default sub-agent should inherit parent mode 'default'; got %q", snap.Header.Mode)
	}
}

func TestAgentExecute_RejectsPermissionEscalationAboveParent(t *testing.T) {
	dir := t.TempDir()
	gate := permission.New(permission.ModeAsk)
	roster := agent.NewRoster(0)
	tool := NewAgent(gate, helloProvider(), tools.NewRegistry(), "m", "s").
		WithRoster(roster).
		WithSessionPersistence(dir, "parent")

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":          "x",
		"permission_mode": "bypassPermissions",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil || !res.IsError || !strings.Contains(res.Output, "cannot be more permissive") {
		t.Fatalf("permission escalation result = %+v", res)
	}
	if _, statErr := os.Stat(filepath.Join(dir, agent.SubAgentTranscriptDirname)); !os.IsNotExist(statErr) {
		t.Fatalf("rejected child created transcript state: %v", statErr)
	}
	if gate.Mode() != permission.ModeAsk {
		t.Errorf("parent gate leaked override: now %q", gate.Mode())
	}
}

func TestAgentExecute_BypassParentChildRunsWithoutEscalation(t *testing.T) {
	dir := t.TempDir()
	gate := permission.New(permission.ModeBypassPermissions)
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
	if snap.Header.Mode != "bypassPermissions" {
		t.Fatalf("inherited child mode = %q, want bypassPermissions", snap.Header.Mode)
	}
}

func TestAgentFullAccessParentRejectsExplicitLowerMode(t *testing.T) {
	for _, childMode := range []permission.Mode{
		permission.ModeDefault,
		permission.ModeAcceptEdits,
		permission.ModePlan,
		permission.ModeDontAsk,
		permission.ModeBypassPermissions,
	} {
		t.Run(string(childMode), func(t *testing.T) {
			dir := t.TempDir()
			gate := permission.New(permission.ModeFullAccess)
			tool := NewAgent(gate, helloProvider(), tools.NewRegistry(), "m", "s").
				WithRoster(agent.NewRoster(0)).
				WithSessionPersistence(dir, "parent")
			in := map[string]any{
				"prompt":          "x",
				"permission_mode": string(childMode),
			}

			if got, reason := tool.CanUse(context.Background(), in); got != tools.PermissionDeny || !strings.Contains(reason, "cannot safely reduce a fullAccess parent") {
				t.Fatalf("CanUse = (%v, %q), want explicit false-downgrade denial", got, reason)
			}
			res, err := tool.Execute(context.Background(), in)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res == nil || !res.IsError || !strings.Contains(res.Output, "cannot safely reduce a fullAccess parent") {
				t.Fatalf("Execute result = %+v, want explicit false-downgrade error", res)
			}
			if _, statErr := os.Stat(filepath.Join(dir, agent.SubAgentTranscriptDirname)); !os.IsNotExist(statErr) {
				t.Fatalf("rejected child created transcript state: %v", statErr)
			}
			if gate.Mode() != permission.ModeFullAccess {
				t.Fatalf("parent gate changed to %q", gate.Mode())
			}
		})
	}
}

func TestAgentExecute_FullAccessParentInheritsWhenModeOmitted(t *testing.T) {
	dir := t.TempDir()
	gate := permission.New(permission.ModeFullAccess)
	tool := NewAgent(gate, helloProvider(), tools.NewRegistry(), "m", "s").
		WithRoster(agent.NewRoster(0)).
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
	if snap.Header.Mode != string(permission.ModeFullAccess) {
		t.Fatalf("inherited child mode = %q, want fullAccess", snap.Header.Mode)
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
