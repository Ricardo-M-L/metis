package builtin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestSubAgentDescriptionsIncludeQueuedCancellation(t *testing.T) {
	if description := (SubAgentList{}).Description(); !strings.Contains(description, "queued/running/completed/failed/killed") || !strings.Contains(description, "SubAgentStop") {
		t.Fatalf("list description omits queued state or cancellation: %q", description)
	}
	if description := (SubAgentStop{}).Description(); !strings.Contains(description, "queued or running") || !strings.Contains(description, "execution-capacity waits") {
		t.Fatalf("stop description omits queued cancellation: %q", description)
	}
}

func TestAgentBackgroundHandshakeReportsRunningSnapshot(t *testing.T) {
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOT_DIR", "")
	roster := agent.NewRoster(1)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := roster.CancelAndWait(ctx); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	tool := NewAgent(permission.New(permission.ModeBypass), &hangingProvider{}, tools.NewRegistry(), "model", "system").WithRoster(roster)
	result, err := tool.Execute(context.Background(), map[string]any{"prompt": "remain running", "run_in_background": true})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("background spawn: %+v, %v", result, err)
	}
	if result.Meta["status"] != "running" || !strings.Contains(result.Output, "status=running") {
		t.Fatalf("handshake status differs from running snapshot: %+v", result)
	}
}
