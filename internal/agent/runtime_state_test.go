package agent

import "testing"

func TestRuntimeStateSnapshotRenderIsByteStable(t *testing.T) {
	snapshot := RuntimeStateSnapshot{
		PermissionMode:   "bypassPermissions",
		WorkingDirectory: "/work/metis",
		SessionID:        "session-1",
		Provider:         "anthropic",
		Model:            "claude-test",
		PlanMode:         true,
		CurrentPlan:      "  verify cache\nship  ",
	}
	const want = "<runtime_state>\n" +
		"permission_mode: bypassPermissions\n" +
		"working_directory: /work/metis\n" +
		"session_id: session-1\n" +
		"provider: anthropic\n" +
		"model: claude-test\n" +
		"plan_mode: true\n" +
		"<current_plan>\nverify cache\nship\n</current_plan>\n" +
		"</runtime_state>"
	if got := snapshot.Render(); got != want {
		t.Fatalf("Render() = %q\nwant     = %q", got, want)
	}
	if got := snapshot.Render(); got != want {
		t.Fatalf("second Render() drifted: %q", got)
	}
}
