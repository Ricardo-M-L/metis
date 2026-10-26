package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
)

func TestCheckpointStatusPollRendersLiveSwitchAndRejectsStaleWorkspace(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	model := minimalModel(200000)
	model.sessionID = "current"
	manager := checkpoint.NewManager("current", t.TempDir(), t.TempDir())
	model.loop.SetCheckpointer(manager)
	state := manager.ControlStatus()
	model.Update(checkpointStatusMsg{manager: manager, sessionID: "current", state: state})
	if !strings.Contains(stripANSI(renderStatusBar(model)), "检查点：开") {
		t.Fatal("CLI status does not show on")
	}
	state.Enabled = false
	model.Update(checkpointStatusMsg{manager: manager, sessionID: "old-session", state: state})
	if !model.checkpointState.Enabled {
		t.Fatal("stale session changed checkpoint status")
	}
	model.Update(checkpointStatusMsg{manager: manager, sessionID: "current", state: state})
	if !strings.Contains(stripANSI(renderStatusBar(model)), "检查点：关") {
		t.Fatal("CLI status did not reflect shared off state")
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
