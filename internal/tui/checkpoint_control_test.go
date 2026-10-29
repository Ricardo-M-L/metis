package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
)

func TestCheckpointStatusPollKeepsSessionStateWithoutFooterLabel(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	model := minimalModel(200000)
	model.sessionID = "current"
	manager := checkpoint.NewManager("current", t.TempDir(), t.TempDir())
	model.loop.SetCheckpointer(manager)
	state := manager.ControlStatus()
	model.Update(checkpointStatusMsg{manager: manager, sessionID: "current", state: state})
	if model.checkpointState == nil || !model.checkpointState.Enabled {
		t.Fatal("CLI did not retain the shared on state")
	}
	if bar := stripANSI(renderStatusBar(model)); strings.Contains(bar, "检查点") || !strings.Contains(bar, "❉ curren") {
		t.Fatalf("footer must omit checkpoint status and retain session identity: %s", bar)
	}
	state.Enabled = false
	model.Update(checkpointStatusMsg{manager: manager, sessionID: "old-session", state: state})
	if !model.checkpointState.Enabled {
		t.Fatal("stale session changed checkpoint status")
	}
	model.Update(checkpointStatusMsg{manager: manager, sessionID: "current", state: state})
	if model.checkpointState.Enabled {
		t.Fatal("CLI did not retain the shared off state")
	}
	if bar := stripANSI(renderStatusBar(model)); strings.Contains(bar, "检查点") || !strings.Contains(bar, "❉ curren") {
		t.Fatalf("off footer must omit checkpoint status and retain session identity: %s", bar)
	}
}

func TestCheckpointAsyncCommandKeepsOriginalWorkspaceAfterNavigation(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	model := minimalModel(200000)
	model.ctx = context.Background()
	model.sessionID = "original"
	original := checkpoint.NewManager("original", t.TempDir(), t.TempDir())
	model.loop.SetCheckpointer(original)
	command := model.checkpointControlCommand("off")
	next := checkpoint.NewManager("next", t.TempDir(), t.TempDir())
	model.loop.SetCheckpointer(next)
	model.sessionID = "next"
	result := command()
	model.Update(result)
	if original.ControlStatus().Enabled || !next.ControlStatus().Enabled {
		t.Fatal("async command changed the newly selected workspace")
	}
	if len(model.messages) != 0 {
		t.Fatal("old workspace command result leaked into new transcript")
	}
}
