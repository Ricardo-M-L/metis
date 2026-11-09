package runtime

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestTraceStreamingPreservesLongTextAndWhitespace(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a := NewTraceAdapter(store)
	a.SetSession("stream")
	want := "  start\n" + strings.Repeat("正文 ", 3000) + "\n  end  \n"
	for _, part := range []string{want[:100], want[100 : len(want)-3], want[len(want)-3:]} {
		a.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: part})
	}
	a.OnEvent(agent.Event{Kind: agent.EventTurnEnd})
	events := store.Events("stream")
	if len(events) != 2 || events[0].Text != want {
		t.Fatalf("stream text lost content: rows=%d got=%d bytes want=%d bytes", len(events), len(events[0].Text), len(want))
	}
}

func TestTraceStreamingEscapedControlsSurviveReload(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := NewTraceAdapter(store)
	a.SetSession("controls")
	a.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: strings.Repeat("\x00", maxTraceStreamBytes)})
	a.OnEvent(agent.Event{Kind: agent.EventTurnEnd})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	events := reloaded.Events("controls")
	if len(events) != 2 || !events[0].Truncated || events[1].Kind != "turn_end" {
		t.Fatalf("encoded oversized row hid its successor: rows=%d", len(events))
	}
}
