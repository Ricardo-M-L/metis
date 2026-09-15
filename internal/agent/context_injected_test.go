package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/security"
)

func TestInjectedNotificationExposesFullContextEnvelope(t *testing.T) {
	inbox := make(chan PeerMessage, 1)
	inbox <- PeerMessage{From: "reviewer", Body: "the complete detail must reach the trace"}
	loop := &Loop{PeerInbox: inbox}
	out := make(chan Event, 8)
	loop.injectPeerMessages(context.Background(), out)
	body := loop.History()[0].Content[0].Text
	for len(out) > 0 {
		ev := <-out
		if ev.Kind == EventContextInjected && ev.ContextText == body && ev.Source == "peer" {
			return
		}
	}
	t.Fatal("notification entered model history, but emitted events do not contain its complete envelope")
}

func TestNotificationSourcesEmitOneCompleteContextEach(t *testing.T) {
	for _, source := range []string{"peer", "job", "dream", "subagent", "monitor"} {
		t.Run(source, func(t *testing.T) {
			loop := &Loop{}
			out := make(chan Event, 8)
			ctx := WithTraceInvocationID(WithParentToolUseID(context.Background(), "parent-tool"), "child-invocation")
			switch source {
			case "peer":
				inbox := make(chan PeerMessage, 1)
				inbox <- PeerMessage{From: "reviewer", Body: "full peer detail"}
				loop.PeerInbox = inbox
				loop.injectPeerMessages(ctx, out)
			case "job":
				loop.injectJobNotificationBatchWithOutputs(ctx, out, []jobs.Notification{{JobID: "job-one", Status: jobs.StatusCompleted}}, map[string]string{"job-one": "full output detail"})
			case "dream":
				inbox := make(chan DreamNotification, 1)
				inbox <- DreamNotification{FilesTouched: []string{"memory.md"}}
				loop.DreamNotify = inbox
				loop.injectDreamNotifications(ctx, out)
			case "subagent":
				loop.subAgentNotify = make(chan SubAgentNotification, 1)
				loop.subAgentNotify <- SubAgentNotification{Name: "reviewer", Summary: "full review detail"}
				loop.injectSubAgentNotifications(ctx, out)
			case "monitor":
				loop.Monitors = NewMonitorRegistry(1)
				loop.Monitors.events <- MonitorEvent{JobID: "job-one", Match: "full match detail"}
				loop.injectMonitorEvents(ctx, out)
			}
			history := loop.History()
			if len(history) != 1 || history[0].Content[0].Synthetic {
				t.Fatal("injection changed history shape or provider Synthetic semantics")
			}
			contexts, infos := 0, 0
			for len(out) > 0 {
				ev := <-out
				switch ev.Kind {
				case EventContextInjected:
					contexts++
					if ev.ContextText != security.RedactSubprocessText(history[0].Content[0].Text) || ev.Source != source {
						t.Fatalf("context does not match full %s envelope", source)
					}
					if ev.TraceInvocationID != "child-invocation" || ev.SubAgentParentID != "parent-tool" {
						t.Fatal("context lost child origin")
					}
				case EventInfo:
					infos++
				}
			}
			if contexts != 1 || infos != 1 {
				t.Fatalf("context=%d info=%d, want exactly one each", contexts, infos)
			}
		})
	}
}

func TestInjectedContextTracesWithoutConsumerAndAfterCancellation(t *testing.T) {
	traceHookMu.RLock()
	previous := traceHook
	traceHookMu.RUnlock()
	t.Cleanup(func() { SetTraceHook(previous) })
	loop := &Loop{}
	var traced []Event
	SetTraceHook(func(ev Event) {
		// A hook may read history: append must release Loop.mu before tracing.
		_ = loop.History()
		traced = append(traced, ev)
	})
	canary := "ghp_" + strings.Repeat("A", 36)
	loop.appendInjectedMessage(nil, nil, canary, "api_key=example-secret "+canary)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := make(chan Event, 1)
	out <- Event{Kind: EventInfo}
	loop.appendInjectedMessage(ctx, out, "job", "completed after cancellation")
	if len(traced) != 2 {
		t.Fatalf("context trace count = %d, want 2", len(traced))
	}
	if traced[0].Source != "context" || strings.Contains(traced[0].ContextText, canary) || strings.Contains(traced[0].ContextText, "example-secret") {
		t.Fatal("context presentation leaked text or arbitrary source")
	}
	if !strings.Contains(loop.History()[0].Content[0].Text, canary) {
		t.Fatal("presentation redaction changed model history")
	}
}

func TestInjectedContextConcurrentHistoryReaders(t *testing.T) {
	loop := &Loop{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			loop.appendInjectedMessage(context.Background(), nil, "job", "finished")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = loop.History()
		}
	}()
	wg.Wait()
	if len(loop.History()) != 100 {
		t.Fatal("concurrent reads lost injected messages")
	}
}
