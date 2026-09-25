package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
)

func TestDesktopWorkerStatusPreservesFinalSubagentAfterCleanup(t *testing.T) {
	input, parent := io.Pipe()
	defer parent.Close()
	var output bytes.Buffer
	bridge := newDesktopWorkerBridge(context.Background(), input, &output)
	defer bridge.Close()
	roster := agent.NewRoster(4)
	stop := bridge.startStatus(roster, nil)
	teammate := &agent.Teammate{Name: "review", AgentID: "agt-status", Started: time.Now(), Status: agent.StatusRunning}
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	teammate.AppendText("first streamed output")
	teammate.Finish(agent.StatusCompleted, "final reply", nil, "end_turn")
	roster.UnregisterTeammate(teammate)
	stop(func() {
		if err := roster.CancelAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	decoder := desktopipc.NewDecoder(&output)
	var latest *desktopipc.Status
	for {
		message, err := decoder.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		latest = message.Status
	}
	if latest == nil || latest.SubAgents != 0 || len(latest.Agents) != 1 {
		t.Fatalf("final status lost child: %+v", latest)
	}
	child := latest.Agents[0]
	if child.Status != "completed" || child.Output != "first streamed output" || child.Result != "final reply" {
		t.Fatalf("bad final child: %+v", child)
	}
}

func TestDesktopWorkerStatusTruncatesLongChildText(t *testing.T) {
	text := strings.Repeat("中", desktopWorkerStatusTextLimit) + "TAIL"
	got, truncated := desktopWorkerStatusText(text)
	if !truncated || len([]rune(got)) > desktopWorkerStatusTextLimit || !strings.HasSuffix(got, "TAIL") {
		t.Fatal("long output was not safely head/tail bounded")
	}
}
