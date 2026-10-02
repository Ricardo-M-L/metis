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
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestDesktopWorkerStatusPreservesFinalSubagentAfterCleanup(t *testing.T) {
	input, parent := io.Pipe()
	defer parent.Close()
	var output bytes.Buffer
	bridge := newDesktopWorkerBridge(context.Background(), input, &output)
	defer bridge.Close()
	roster := agent.NewRoster(4)
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.ContextWindow = 128_000
	loop.AppendUser(strings.Repeat("live worker prompt ", 80))
	stop := bridge.startStatus(roster, nil, loop)
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
	frames := 0
	for {
		message, err := decoder.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		latest = message.Status
		frames++
		if frames == 1 && (latest == nil || latest.Context == nil || latest.Context.Used <= 0) {
			t.Fatalf("running status lacked worker pressure: %+v", latest)
		}
	}
	if frames < 2 || latest == nil || latest.SubAgents != 0 || len(latest.Agents) != 1 {
		t.Fatalf("final status lost child: %+v", latest)
	}
	if latest.Context == nil || latest.Context.Used <= 0 || latest.Context.Window != 128_000 {
		t.Fatalf("final status lost worker context pressure: %+v", latest.Context)
	}
	child := latest.Agents[0]
	if child.Status != "completed" || child.Output != "first streamed output" || child.Result != "final reply" {
		t.Fatalf("bad final child: %+v", child)
	}
}

func TestDesktopWorkerStatusSamplesLoopPressureAndKeepsFinalReading(t *testing.T) {
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.ContextWindow = 128_000
	loop.AppendUser(strings.Repeat("worker context ", 100))
	sampler := desktopWorkerStatusSampler{loop: loop, known: make(map[string]*agent.Teammate)}
	first := sampler.snapshot(false)
	if first.Context == nil || first.Context.Used <= 0 || first.Context.Window != 128_000 {
		t.Fatalf("running worker has no real pressure: %+v", first.Context)
	}
	loop.AppendUser(strings.Repeat("new turn ", 200))
	latest := sampler.snapshot(false)
	if latest.Context == nil || latest.Context.Used <= first.Context.Used {
		t.Fatalf("worker pressure did not advance: first=%+v latest=%+v", first.Context, latest.Context)
	}
	// Cleanup can release the runtime before the final IPC frame. That frame
	// must preserve the last sample instead of publishing a fabricated zero.
	sampler.loop = nil
	final := sampler.snapshot(true)
	if final.Context == nil || *final.Context != *latest.Context {
		t.Fatalf("final worker pressure was lost: latest=%+v final=%+v", latest.Context, final.Context)
	}
}

func TestDesktopWorkerStatusTruncatesLongChildText(t *testing.T) {
	text := strings.Repeat("中", desktopWorkerStatusTextLimit) + "TAIL"
	got, truncated := desktopWorkerStatusText(text)
	if !truncated || len([]rune(got)) > desktopWorkerStatusTextLimit || !strings.HasSuffix(got, "TAIL") {
		t.Fatal("long output was not safely head/tail bounded")
	}
}

func TestDesktopWorkerStatusSeparatesLifecycleFromHeldExecution(t *testing.T) {
	config := agent.DesktopExecutionConfig{SlotDir: t.TempDir(), TotalAgentSlots: 1, SubagentsPerRoot: 1, Owner: "status-test"}
	ctx := agent.WithDesktopExecutionConfig(context.Background(), config)
	roster := agent.NewRoster(1, 1)
	child := &agent.Teammate{Name: "worker"}
	if err := agent.RegisterDesktopTeammate(ctx, roster, child, false); err != nil {
		t.Fatal(err)
	}
	defer roster.UnregisterTeammate(child)
	sampler := desktopWorkerStatusSampler{roster: roster, known: make(map[string]*agent.Teammate)}
	queued := sampler.snapshot(false).Agents[0]
	if queued.Status != "queued" || queued.ExecutionPhase != "queued" || queued.HoldsExecutionSlot {
		t.Fatalf("queued status=%+v", queued)
	}
	if _, err := agent.AcquireDesktopTeammateExecution(ctx, roster, child); err != nil {
		t.Fatal(err)
	}
	running := sampler.snapshot(false).Agents[0]
	if running.Status != "running" || running.ExecutionPhase != "executing" || !running.HoldsExecutionSlot {
		t.Fatalf("running status=%+v", running)
	}
	child.Finish(agent.StatusCompleted, "done", nil, "end_turn")
	roster.UnregisterTeammate(child)
	finished := sampler.snapshot(true).Agents[0]
	if finished.Status != "completed" || finished.ExecutionPhase != "finished" || finished.HoldsExecutionSlot {
		t.Fatalf("finished status=%+v", finished)
	}
}
