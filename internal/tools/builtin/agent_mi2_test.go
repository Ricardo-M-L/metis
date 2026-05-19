package builtin

// agent_mi2_test.go — covers the 2026-05-18 resume_from liveness
// check. Previously a `resume_from=<id>` call would proceed even if
// the source sub-agent was still RUNNING, producing two parallel
// loops sharing the same AgentID + transcript file (undefined
// behavior; the agent.go schema warned about it but didn't enforce).
//
// The fix: if Roster.LookupByAgentID finds the id and Status is
// StatusRunning, refuse with a clear "stop it first" error.

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestAgentExecute_ResumeFromLiveAgent_Refused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	roster := agent.NewRoster(0)

	// Plant a live teammate in the roster directly. We don't need a
	// real running sub-loop; the resume check reads Status from the
	// snapshot and that's StatusRunning by default for a fresh
	// Teammate.
	live := &agent.Teammate{
		Name:    "alice",
		AgentID: "agt-livedude",
	}
	if err := roster.Register(live); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { roster.Unregister(live.Name) })

	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "m", "s").
		WithRoster(roster).
		WithSessionPersistence(dir, "parent")

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":      "continue please",
		"resume_from": "agt-livedude",
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on live-agent resume; got success: %s", res.Output)
	}
	if !strings.Contains(res.Output, "still RUNNING") {
		t.Errorf("error should mention RUNNING status; got %q", res.Output)
	}
	if !strings.Contains(res.Output, "SubAgentStop") {
		t.Errorf("error should hint at SubAgentStop; got %q", res.Output)
	}
}

// Sanity: resume against a NON-running (finished) teammate should
// fall through to the normal "transcript not found" path, NOT the
// liveness refusal. Otherwise the recentlyFinished LRU would
// permanently block resume after legitimate completion.
func TestAgentExecute_ResumeFromFinishedAgent_PassesLivenessCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	roster := agent.NewRoster(0)

	completed := &agent.Teammate{
		Name:    "bob",
		AgentID: "agt-done",
	}
	if err := roster.Register(completed); err != nil {
		t.Fatalf("Register: %v", err)
	}
	completed.Finish(agent.StatusCompleted, "done", nil, "ok")
	roster.Unregister(completed.Name) // moves to recentlyFinished

	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "m", "s").
		WithRoster(roster).
		WithSessionPersistence(dir, "parent")

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":      "pick up where you left off",
		"resume_from": "agt-done",
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	// We expect IsError, but the reason should be "transcript not
	// found" (no file was written), NOT the liveness refusal.
	if !res.IsError {
		t.Fatalf("expected IsError on missing transcript; got success: %s", res.Output)
	}
	if strings.Contains(res.Output, "still RUNNING") {
		t.Errorf("finished agent must NOT trigger liveness refusal; got %q", res.Output)
	}
}
