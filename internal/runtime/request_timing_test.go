package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestRequestTimingOwnershipPersistenceZeroAndBurst(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	old := CurrentTraceAdapter()
	a := NewTraceAdapter(store)
	SetTraceAdapter(a)
	defer SetTraceAdapter(old)
	a.SetSession("original")
	ctx, origin := BindTraceTurn(context.Background(), "original")
	id := agent.TraceInvocationIDFromContext(ctx)
	a.OnEvent(agent.Event{Kind: agent.EventRequestTimingBind, TraceInvocationID: "timing-owner", TraceParentInvocationID: id})
	a.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "part1", TraceInvocationID: id})
	at := time.Now()
	r := transport.TimingRecord{Phase: "first_nonempty_text_delta", RequestID: "request-local", ProviderCall: 1, Attempt: 2, ElapsedMS: 0, At: at}
	a.OnEvent(agent.Event{Kind: agent.EventRequestTiming, TraceInvocationID: "timing-owner", RequestTiming: &r})
	if a.burstBuf.String() != "part1" {
		t.Fatal("timing flushed burst")
	}
	a.OnEvent(agent.Event{Kind: agent.EventLoopDone, TraceInvocationID: id})
	a.SetSession("other")
	a.OnEvent(agent.Event{Kind: agent.EventRequestTiming, TraceInvocationID: "timing-owner", RequestTiming: &r})
	a.OnEvent(agent.Event{Kind: agent.EventRequestTimingUnbind, TraceInvocationID: "timing-owner"})
	EndTraceTurn(ctx)
	if len(a.timingOrigins) != 0 {
		t.Fatal("timing owner leaked")
	}
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "original.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"elapsed_ms":0`) {
		t.Fatalf("zero lost: %s", data)
	}
	reloaded, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	var found int
	for _, ev := range reloaded.Events("original") {
		if ev.Kind == "request_timing" {
			found++
			if ev.Turn != origin.Turn || ev.TraceInvocationID != id || !ev.TS.Equal(at) || ev.RequestTiming == nil {
				t.Fatalf("bad persisted event: %+v", ev)
			}
			if ev.RequestTiming.Attempt != 2 {
				t.Fatal(ev)
			}
			if _, err := json.Marshal(ev); err != nil {
				t.Fatal(err)
			}
		}
	}
	if found != 2 {
		t.Fatalf("timing count=%d", found)
	}
	if len(reloaded.Events("other")) != 0 {
		t.Fatal("timing moved session")
	}
}

func TestRequestTimingLegacyChildDoesNotChangeLifecycle(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := NewTraceAdapter(store)
	a.SetSession("parent")
	a.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Agent", ToolUseID: "child"})
	turn := a.activeTurns["parent"]
	a.OnEvent(agent.Event{Kind: agent.EventRequestTimingBind, TraceInvocationID: "owner", SubAgentParentID: "child"})
	a.OnEvent(agent.Event{Kind: agent.EventRequestTiming, TraceInvocationID: "owner", RequestTiming: &transport.TimingRecord{Phase: "pre_request_start", RequestID: "request", At: time.Now()}})
	a.OnEvent(agent.Event{Kind: agent.EventRequestTimingUnbind, TraceInvocationID: "owner"})
	if a.activeTurns["parent"] != turn || len(a.invocationOrigin) != 0 {
		t.Fatal("timing changed ordinary lifecycle")
	}
	evs := store.Events("parent")
	if len(evs) != 2 || evs[1].SubAgentOf != "child" {
		t.Fatalf("child ownership: %+v", evs)
	}
}
