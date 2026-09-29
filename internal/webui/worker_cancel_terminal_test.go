package webui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
)

func TestWorkerCancellationDrainsTerminalStatusAndDurableCleanup(t *testing.T) {
	root := t.TempDir()
	t.Setenv("METIS_TEST_WORKER_ROOT", root)
	runner := protocolTestRunner(t, "cancel-final-status")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var status desktopipc.Status
	result, err := runner.Run(ctx, IsolatedTurnRequest{SessionID: "cancel", WorkDir: root,
		OnStatus: func(value desktopipc.Status) { status = value },
		OnText: func(text string) {
			if text == "ready" {
				cancel()
			}
		},
	})
	if !errors.Is(err, context.Canceled) || !result.Stopped {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if status.SubAgents != 0 || len(status.Agents) != 2 || status.Agents[0].Status != "killed" || status.Agents[1].Status != "killed" {
		t.Fatalf("final status was lost: %+v", status)
	}
	if terminal, err := os.ReadFile(filepath.Join(root, "terminal")); err != nil || string(terminal) != "killed" {
		t.Fatalf("durable child cleanup=%q err=%v", terminal, err)
	}
}

func TestWorkerExitRecoversQueuedAndRunningTerminalsForReopenedHistory(t *testing.T) {
	s, store := testServer(t)
	status := desktopipc.Status{SubAgents: 12, NamedAgents: 12}
	for i := 0; i < 14; i++ {
		id := fmt.Sprintf("agt-stop-%02d", i)
		owner := "session-a"
		if i == 13 {
			owner = "session-b"
		}
		transcript, err := agent.NewSubAgentTranscript(store.Dir, id, agent.NewSubAgentHeader(id, "fixture", owner, id, t.TempDir(), "default"))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := transcript.AppendTerminal(agent.SubAgentTerminal{Status: "completed", EndedAt: time.Now(), Output: "durable output", Result: "durable result", StopHint: "end_turn"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := transcript.Close(); err != nil {
			t.Fatal(err)
		}
		if i < 12 {
			state := "running"
			if i >= 8 {
				state = "queued"
			}
			status.Agents = append(status.Agents, desktopipc.Subagent{AgentID: id, Name: id, Status: state, Background: true, StartedAt: time.Now().Add(-time.Second)})
		}
	}
	s.setWorkerSnapshot("session-a", status)
	s.finishWorkerSnapshot("session-a", true, true)
	finished, ok := s.workerSnapshot("session-a")
	if !ok || finished.SubAgents != 0 || finished.NamedAgents != 0 || len(finished.Agents) != 13 {
		t.Fatalf("finished snapshot=%+v", finished)
	}
	restarted := NewServer("127.0.0.1:0", nil, store)
	for i := 0; i < 13; i++ {
		id := fmt.Sprintf("agt-stop-%02d", i)
		want := "killed"
		if i == 0 {
			want = "completed"
		}
		for _, current := range []*Server{s, restarted} {
			view, found := current.subAgentDetailView("session-a", id)
			if !found || view.Status != want || view.EndedAt.IsZero() {
				t.Fatalf("reopened %s=%+v found=%v", id, view, found)
			}
			if i == 0 && (view.Output != "durable output" || view.Result != "durable result") {
				t.Fatalf("durable successful result overwritten: %+v", view)
			}
		}
	}
	foreign, err := agent.LoadSubAgentSnapshot(store.Dir, "agt-stop-13")
	if err != nil || foreign.Terminal != nil {
		t.Fatalf("foreign session mutated=%+v err=%v", foreign, err)
	}
}

func TestWorkerExitWithoutStatusInitializesEmptyTerminalSnapshot(t *testing.T) {
	s, _ := testServer(t)
	s.finishWorkerSnapshot("empty", true, true)
	if status, ok := s.workerSnapshot("empty"); !ok || status.SubAgents != 0 {
		t.Fatalf("empty worker status=%+v exists=%v", status, ok)
	}
}
