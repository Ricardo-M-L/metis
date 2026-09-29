package builtin

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type desktopQueueProvider struct {
	llm.Provider
	calls atomic.Int32
}

func (p *desktopQueueProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	p.calls.Add(1)
	return p.Provider.Stream(ctx, req)
}

type desktopQueueResult struct {
	result *tools.Result
	err    error
}

func desktopQueueFixture(t *testing.T) (Agent, *agent.Roster, *desktopQueueProvider, func()) {
	t.Helper()
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOT_DIR", t.TempDir())
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOTS", "1")
	t.Setenv("METIS_DESKTOP_SUBAGENT_CAP", "1")
	release, err := agent.AcquireDesktopSubagentSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	roster := agent.NewRoster(1)
	t.Cleanup(func() {
		roster.CancelAll()
		waitForRosterCount(t, roster, 0, 3*time.Second)
	})
	provider := &desktopQueueProvider{Provider: helloProvider()}
	tool := NewAgent(permission.New(permission.ModeBypass), provider, tools.NewRegistry(), "model", "system").WithRoster(roster)
	return tool, roster, provider, release
}

func desktopQueueSpawn(t *testing.T, tool Agent, background bool, parents ...context.Context) <-chan desktopQueueResult {
	t.Helper()
	parent := context.Background()
	if len(parents) > 0 {
		parent = parents[0]
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	t.Cleanup(cancel)
	finished := make(chan desktopQueueResult, 1)
	go func() {
		res, err := tool.Execute(ctx, map[string]any{"prompt": "inspect", "run_in_background": background})
		finished <- desktopQueueResult{res, err}
	}()
	return finished
}

func desktopQueueHandshake(t *testing.T, finished <-chan desktopQueueResult, release func()) *tools.Result {
	t.Helper()
	select {
	case got := <-finished:
		if got.err != nil || got.result == nil || got.result.IsError {
			t.Fatalf("background admission = %+v, %v", got.result, got.err)
		}
		return got.result
	case <-time.After(500 * time.Millisecond):
		release()
		<-finished
		t.Fatal("saturated background Agent blocked instead of returning a queued ID")
		return nil
	}
}

func TestAgentDesktopSaturatedBackgroundReturnsQueuedID(t *testing.T) {
	tool, roster, provider, release := desktopQueueFixture(t)
	sessionDir := t.TempDir()
	tool = tool.WithSessionPersistence(sessionDir, "parent-session")
	events := make(chan agent.Event, 32)
	res := desktopQueueHandshake(t, desktopQueueSpawn(t, tool, true, agent.WithEventOut(context.Background(), events)), release)
	id, _ := res.Meta["agent_id"].(string)
	teammate, ok := roster.LookupByAgentID(id)
	if id == "" || !ok || teammate.Snapshot().Status.String() != "queued" {
		t.Fatalf("missing queued identity: result=%+v teammate=%+v", res, teammate)
	}
	if provider.calls.Load() != 0 {
		t.Fatal("queued child reached provider without a permit")
	}
	if res.Meta["status"] != "queued" || res.Presentation["subagent"].(map[string]any)["status"] != "queued" {
		t.Fatalf("handshake lost queued presentation: %+v", res)
	}
	if !strings.Contains(res.Output, "status=queued") || !strings.Contains(res.Output, "start automatically") {
		t.Fatalf("model-facing handshake lost queue status: %q", res.Output)
	}
	release()
	waitForRosterCount(t, roster, 0, 3*time.Second)
	if provider.calls.Load() != 1 || teammate.Snapshot().Status != agent.StatusCompleted {
		t.Fatalf("after admission calls=%d status=%s", provider.calls.Load(), teammate.Snapshot().Status)
	}
	var states []string
	for len(events) > 0 {
		ev := <-events
		if ev.Kind == agent.EventSubAgentStart || ev.Kind == agent.EventSubAgentEnd {
			states = append(states, ev.SubAgentStatus)
		}
	}
	if !reflect.DeepEqual(states, []string{"queued", "running", "completed"}) {
		t.Fatalf("lifecycle transitions = %v", states)
	}
	snapshot, err := agent.LoadSubAgentSnapshot(sessionDir, id)
	if err != nil || snapshot.Terminal == nil || snapshot.Terminal.Status != "completed" {
		t.Fatalf("durable queue completion = %+v, %v", snapshot, err)
	}
}

func TestAgentDesktopQueuedStopDoesNotStartProvider(t *testing.T) {
	tool, roster, provider, release := desktopQueueFixture(t)
	sessionDir := t.TempDir()
	tool = tool.WithSessionPersistence(sessionDir, "parent-session")
	res := desktopQueueHandshake(t, desktopQueueSpawn(t, tool, true), release)
	id := res.Meta["agent_id"].(string)
	stop, err := NewSubAgentStop(permission.New(permission.ModeBypass), roster).Execute(context.Background(), map[string]any{"agent_id": id})
	if err != nil || stop.IsError {
		t.Fatalf("stop queued child = %+v, %v", stop, err)
	}
	waitForRosterCount(t, roster, 0, 3*time.Second)
	teammate, _ := roster.LookupByAgentID(id)
	if teammate.Snapshot().Status != agent.StatusKilled || provider.calls.Load() != 0 {
		t.Fatalf("cancelled queue status=%s provider calls=%d", teammate.Snapshot().Status, provider.calls.Load())
	}
	snapshot, loadErr := agent.LoadSubAgentSnapshot(sessionDir, id)
	if loadErr != nil || snapshot.Terminal == nil || snapshot.Terminal.Status != "killed" {
		t.Fatalf("durable queued cancellation = %+v, %v", snapshot, loadErr)
	}
	release()
	got, err := tool.Execute(context.Background(), map[string]any{"prompt": "next"})
	if err != nil || got.IsError || provider.calls.Load() != 1 {
		t.Fatalf("permit after queued cancel result=%+v err=%v calls=%d", got, err, provider.calls.Load())
	}
}

func TestAgentDesktopDescriptionDoesNotDependOnProcessEnvironment(t *testing.T) {
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOT_DIR", t.TempDir())
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOTS", "16")
	t.Setenv("METIS_DESKTOP_SUBAGENT_CAP", "8")
	text := (Agent{}).Description()
	for _, want := range []string{"Desktop queues excess work", "background calls return IDs", "queued work runs automatically", "CLI uses configured limits"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Description missing %q", want)
		}
	}
	for _, stale := range []string{"20 named + 40 anonymous", "16 execution slots", "8 child executions per root"} {
		if strings.Contains(text, stale) {
			t.Fatalf("description inferred per-turn capacity from process environment: %q", stale)
		}
	}
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOT_DIR", "")
	if (Agent{}).Description() != text {
		t.Fatal("generic description changed when Desktop process environment was removed")
	}
}

func TestAgentDesktopForegroundWaitsForPermitAndResult(t *testing.T) {
	tool, roster, provider, release := desktopQueueFixture(t)
	finished := desktopQueueSpawn(t, tool, false)
	deadline := time.After(500 * time.Millisecond)
	for roster.Count() == 0 {
		select {
		case <-deadline:
			release()
			<-finished
			t.Fatal("foreground wait has no cancellable queued identity")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case got := <-finished:
		t.Fatalf("foreground returned before permit was available: %+v", got)
	default:
	}
	if provider.calls.Load() != 0 {
		t.Fatal("foreground provider started before admission")
	}
	release()
	select {
	case got := <-finished:
		if got.err != nil || got.result.IsError || got.result.Output != "sub-agent done" {
			t.Fatalf("foreground result = %+v, %v", got.result, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreground did not finish after admission")
	}
}
