package webui

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// The hub must deliver the session each event belongs to: a tab viewing
// session A filters out the live stream of a turn in session B.
func TestEventHubPublishCarriesSession(t *testing.T) {
	h := newEventHub()
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	h.publish("sess-1", agent.Event{Kind: agent.EventTextDelta, TextDelta: "hello"})

	select {
	case he := <-ch:
		if he.session != "sess-1" || he.ev.TextDelta != "hello" {
			t.Fatalf("wrong hub event: %+v", he)
		}
	default:
		t.Fatal("subscriber should receive the published event")
	}
}

// Slow subscribers (full buffer) must not block the publishing turn.
func TestEventHubSlowSubscriberDrops(t *testing.T) {
	h := newEventHub()
	ch := h.subscribe() // buffer 256, never drained
	defer h.unsubscribe(ch)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			h.publish("s", agent.Event{Kind: agent.EventInfo})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publish must not block on a slow subscriber")
	}
}

func TestEventHubReplaysAfterCursorAndDetectsExpiredWindow(t *testing.T) {
	h := newEventHub()
	h.replayCap = 2
	h.publish("s", agent.Event{Kind: agent.EventTextDelta, TextDelta: "one"})
	h.publish("s", agent.Event{Kind: agent.EventTextDelta, TextDelta: "two"})
	h.publish("s", agent.Event{Kind: agent.EventTextDelta, TextDelta: "three"})
	h.publish("s", agent.Event{Kind: agent.EventTextDelta, TextDelta: "four"})

	ch, replay, reset := h.subscribeFrom(3)
	defer h.unsubscribe(ch)
	if reset || len(replay) != 1 || replay[0].sequence != 4 || replay[0].ev.TextDelta != "four" {
		t.Fatalf("replay = %+v reset=%v", replay, reset)
	}
	old, _, reset := h.subscribeFrom(1)
	defer h.unsubscribe(old)
	if !reset {
		t.Fatal("cursor older than bounded replay window should request reset")
	}
}

func TestHandleEventsStreamsSessionAndFields(t *testing.T) {
	s, _ := testServer(t)
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	// The ready frame arrives first; the handler then waits for events.
	reader := bufio.NewReader(res.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "event: ready") {
		t.Fatalf("first frame should be ready, got %q", line)
	}

	// Publish a tool event and read until its payload frame shows up.
	s.hub.publish("sess-42", agent.Event{
		Kind:      agent.EventToolStart,
		ToolName:  "Bash",
		ToolUseID: "tu-1",
	})

	deadline := time.Now().Add(3 * time.Second)
	var got map[string]any
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(line), "data: ")), &got); err != nil {
			continue
		}
		if got["tool"] == "Bash" {
			break
		}
	}
	if got["tool"] != "Bash" {
		t.Fatal("tool_start event not streamed")
	}
	if got["session"] != "sess-42" {
		t.Fatalf("session missing/wrong in payload: %v", got)
	}
	if got["id"] != "tu-1" {
		t.Fatalf("toolUseId missing/wrong in payload: %v", got)
	}
}
