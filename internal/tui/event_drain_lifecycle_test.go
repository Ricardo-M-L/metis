package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestActiveScreenSpinnerTickKeepsTurnLifecycleAlive(t *testing.T) {
	m := newSlashTestModel(t)
	m.eventCh = make(chan agent.Event, 8)
	m.doneCh = make(chan error, 1)
	m.turnActive = true
	m.spinnerActive = true
	m.spinnerPhase = "requesting"
	m.spinnerStartedAt = time.Now()
	m.openAgentsView()
	opened := m.activeScreen

	m.eventCh <- agent.Event{Kind: agent.EventTextDelta, TextDelta: "screen-tick-"}
	_, cmd := m.Update(spinnerTick{})
	if got := m.streamingText; got != "screen-tick-" {
		t.Fatalf("active screen swallowed spinner event drain: %q", got)
	}
	if m.activeScreen != opened {
		t.Fatalf("lifecycle tick changed active screen: %T -> %T", opened, m.activeScreen)
	}
	if cmd == nil {
		t.Fatal("active turn with open screen did not re-arm spinner tick")
	}
	if _, ok := cmd().(spinnerTick); !ok {
		t.Fatalf("re-arm command returned a non-spinner message")
	}

	// doneCh may become ready while the screen remains open. Its final event
	// must be applied before finalizeTurn flushes the response.
	m.eventCh <- agent.Event{Kind: agent.EventTextDelta, TextDelta: "tail"}
	m.doneCh <- nil
	m.Update(spinnerTick{})
	if m.turnActive || m.spinnerActive {
		t.Fatal("turn did not finalize while /agents-view remained open")
	}
	if m.activeScreen != opened {
		t.Fatalf("turn completion unexpectedly closed active screen: %T", m.activeScreen)
	}
	if got := lastAssistantContent(m.messages); got != "screen-tick-tail" {
		t.Fatalf("final response lost an event behind active screen: %q", got)
	}
}

func TestDoneReceiptDrainsLateForwardedTailBeforeFinalize(t *testing.T) {
	m := newSlashTestModel(t)
	m.eventCh = make(chan agent.Event, 1)
	m.doneCh = make(chan error, 1)
	m.turnActive = true
	m.spinnerActive = true
	m.spinnerStartedAt = time.Now()
	m.streamingText = "head-"

	// Model the cross-channel race directly: Update already received doneCh,
	// while the producer's preceding eventCh send became visible just after
	// the initial empty check. Receiving done guarantees this is a stable tail.
	m.eventCh <- agent.Event{Kind: agent.EventTextDelta, TextDelta: "late-tail"}
	if finalized := m.finalizeTurnAfterForwardedEvents(nil, 0); !finalized {
		t.Fatal("single late tail should fit the remaining event budget")
	}
	if m.turnActive || m.spinnerActive {
		t.Fatal("turn did not finalize after applying stable event tail")
	}
	if len(m.eventCh) != 0 {
		t.Fatalf("late event remained buffered after finalize: %d", len(m.eventCh))
	}
	if got := lastAssistantContent(m.messages); got != "head-late-tail" {
		t.Fatalf("finalize ran before late forwarded tail: %q", got)
	}
}

func TestSpinnerBurstIsBoundedAndDrainsTailBeforeDone(t *testing.T) {
	m := newSlashTestModel(t)
	const eventCount = maxAgentEventsPerUpdate*2 + 7
	m.eventCh = make(chan agent.Event, eventCount)
	m.doneCh = make(chan error, 1)
	m.turnActive = true
	m.spinnerActive = true
	m.spinnerPhase = "responding"
	m.spinnerStartedAt = time.Now()
	m.firstStreamAt = time.Now()

	var want strings.Builder
	for i := 0; i < eventCount; i++ {
		delta := fmt.Sprintf("%03d|", i)
		want.WriteString(delta)
		m.eventCh <- agent.Event{Kind: agent.EventTextDelta, TextDelta: delta}
	}
	m.doneCh <- nil

	_, cmd := m.Update(spinnerTick{})
	wantFirst := want.String()[:maxAgentEventsPerUpdate*4]
	if got := m.streamingText; got != wantFirst {
		t.Fatalf("first tick drained outside its %d-event budget: got %d bytes, want %d",
			maxAgentEventsPerUpdate, len(got), len(wantFirst))
	}
	if !m.turnActive {
		t.Fatal("doneCh finalized before the buffered event tail was handled")
	}
	if len(m.doneCh) != 1 {
		t.Fatal("doneCh was consumed while event backlog remained")
	}
	if cmd == nil {
		t.Fatal("bounded drain did not schedule a quick continuation")
	}

	// A key message must run between bounded drain batches instead of waiting
	// for the provider stream to end. Update may consume one event first, but
	// must still deliver the key to the editor in the same call.
	m.Update(tea.KeyPressMsg{Code: 'z', Text: "z"})
	if got := m.input.Value(); got != "z" {
		t.Fatalf("event backlog starved keyboard input: %q", got)
	}

	next, ok := cmd().(spinnerTick)
	if !ok {
		t.Fatal("backlog continuation did not produce spinnerTick")
	}
	for steps := 0; m.turnActive && steps < 4; steps++ {
		_, cmd = m.Update(next)
		if !m.turnActive {
			break
		}
		if cmd == nil {
			t.Fatal("event backlog remained without another drain command")
		}
		next, ok = cmd().(spinnerTick)
		if !ok {
			t.Fatal("follow-up backlog command did not produce spinnerTick")
		}
	}
	if m.turnActive || m.spinnerActive {
		t.Fatal("turn stayed active after the bounded batches drained")
	}
	if len(m.eventCh) != 0 || len(m.doneCh) != 0 {
		t.Fatalf("lifecycle channels not drained: events=%d done=%d", len(m.eventCh), len(m.doneCh))
	}
	if got := lastAssistantContent(m.messages); got != want.String() {
		t.Fatalf("finalizeTurn ran before the tail event: got %d bytes, want %d",
			len(got), len(want.String()))
	}
}

func lastAssistantContent(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			return messages[i].Content
		}
	}
	return ""
}
