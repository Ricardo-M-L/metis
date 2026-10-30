package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func continuationFixture(t *testing.T, provider llm.Provider) (string, *SubAgentTranscript, *Loop, *Teammate) {
	t.Helper()
	dir := t.TempDir()
	transcript, err := NewDurableSubAgentTranscript(dir, "agt-followup", NewSubAgentHeader("agt-followup", "test-model", "parent", "worker", dir, "default"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypassPermissions), nil, "system", 20)
	loop.AppendUser("original task")
	loop.HistoryCheckpoint = transcript.Checkpoint
	if err := transcript.Checkpoint(loop.History()); err != nil {
		t.Fatal(err)
	}
	teammate := &Teammate{AgentID: "agt-followup", Name: "worker", Status: StatusRunning}
	if err := teammate.BindContinuation("parent", loop, transcript); err != nil {
		t.Fatal(err)
	}
	return dir, transcript, loop, teammate
}

func TestSubAgentContinuationRejectsOtherParentBeforePersistence(t *testing.T) {
	_, transcript, _, teammate := continuationFixture(t, &queuedStreamProvider{})
	if _, err := teammate.SubmitContinuation("other", "must not reach child"); !errors.Is(err, ErrSubAgentContinuationOwner) {
		t.Fatalf("owner error=%v", err)
	}
	if got := transcript.PendingFollowups(); len(got) != 0 {
		t.Fatalf("rejected input persisted: %#v", got)
	}
}

func TestSubAgentContinuationAcceptsQueuedInputBeforeRun(t *testing.T) {
	provider := &queuedStreamProvider{}
	dir, transcript, loop, teammate := continuationFixture(t, provider)
	receipt, err := teammate.SubmitContinuation("parent", "  follow up before admission\n  ")
	if err != nil || receipt.Status != "queued" || !receipt.Durable {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	saved, err := LoadSubAgentSnapshot(dir, teammate.AgentID)
	if err != nil || len(saved.PendingFollowups) != 1 || saved.PendingFollowups[0].ID != receipt.InputID {
		t.Fatalf("unpersisted receipt: %#v %v", saved, err)
	}
	if err := RunSubAgentWithContinuations(context.Background(), make(chan Event, 512), loop, teammate, transcript); err != nil {
		t.Fatal(err)
	}
	requests := provider.capturedRequests()
	if len(requests) != 2 {
		t.Fatalf("requests=%d", len(requests))
	}
	if got := transcript.PendingFollowups(); len(got) != 0 {
		t.Fatalf("consumed input remains pending: %#v", got)
	}
	if _, err := teammate.SubmitContinuation("parent", "too late"); !errors.Is(err, ErrSubAgentContinuationClosed) {
		t.Fatalf("closed error=%v", err)
	}
}

func TestSubAgentContinuationFinalTokenRaceStartsNextRunSameChild(t *testing.T) {
	provider := &queuedStreamProvider{}
	_, transcript, loop, teammate := continuationFixture(t, provider)
	var once sync.Once
	var receipt SubAgentContinuationReceipt
	var submitErr error
	loop.Hooks.Register(LoopEndHandler(func(context.Context, HookContext, string) {
		once.Do(func() { receipt, submitErr = teammate.SubmitContinuation("parent", "arrived after steering closed") })
	}))
	if err := RunSubAgentWithContinuations(context.Background(), make(chan Event, 512), loop, teammate, transcript); err != nil {
		t.Fatal(err)
	}
	if submitErr != nil || receipt.Status != "queued" {
		t.Fatalf("receipt=%#v err=%v", receipt, submitErr)
	}
	requests := provider.capturedRequests()
	if len(requests) != 2 {
		t.Fatalf("final-race input not executed: requests=%d", len(requests))
	}
	last := requests[len(requests)-1]
	count := 0
	for _, message := range last.Messages {
		for _, block := range message.Content {
			count += strings.Count(block.Text, "[parent follow-up "+receipt.InputID+"]")
		}
	}
	if count != 1 {
		t.Fatalf("follow-up delivery count=%d", count)
	}
	if len(transcript.PendingFollowups()) != 0 {
		t.Fatal("final-race input not checkpointed")
	}
}

func TestSubAgentContinuationConcurrentInputsShareOneProducer(t *testing.T) {
	stream := &gatedRegressionStream{started: make(chan struct{}), release: make(chan struct{}), events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "first"}, {Type: "message_delta", StopReason: "end_turn"}, {Type: "message_stop"},
	}}
	provider := &queuedStreamProvider{streams: []llm.StreamReader{stream}}
	_, transcript, loop, teammate := continuationFixture(t, provider)
	done := make(chan error, 1)
	go func() {
		done <- RunSubAgentWithContinuations(context.Background(), make(chan Event, 1024), loop, teammate, transcript)
	}()
	<-stream.started
	if err := RunSubAgentWithContinuations(context.Background(), make(chan Event, 1), loop, teammate, transcript); !errors.Is(err, ErrSubAgentContinuationClosed) {
		t.Fatalf("second producer admitted: %v", err)
	}
	const count = 24
	results := make(chan SubAgentContinuationReceipt, count)
	errorsOut := make(chan error, count)
	var submitters sync.WaitGroup
	for i := 0; i < count; i++ {
		submitters.Add(1)
		go func(i int) {
			defer submitters.Done()
			receipt, err := teammate.SubmitContinuation("parent", fmt.Sprintf("instruction %d", i))
			results <- receipt
			errorsOut <- err
		}(i)
	}
	submitters.Wait()
	close(results)
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for receipt := range results {
		if !receipt.Durable || receipt.Status != "steered" || seen[receipt.InputID] {
			t.Fatalf("bad receipt=%#v", receipt)
		}
		seen[receipt.InputID] = true
	}
	if pending := transcript.PendingFollowups(); len(pending) != count {
		t.Fatalf("pending=%d", len(pending))
	}
	close(stream.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requests := provider.capturedRequests()
	if len(requests) != 2 {
		t.Fatalf("parallel producer or missing steer: requests=%d", len(requests))
	}
	last := requests[len(requests)-1]
	for id := range seen {
		matches := 0
		for _, message := range last.Messages {
			for _, block := range message.Content {
				matches += strings.Count(block.Text, "[parent follow-up "+id+"]")
			}
		}
		if matches != 1 {
			t.Fatalf("input %s occurrences=%d", id, matches)
		}
	}
	if len(transcript.PendingFollowups()) != 0 {
		t.Fatal("concurrent follow-ups remain pending")
	}
}

func TestSubAgentContinuationCancellationKeepsAcceptedInputForResume(t *testing.T) {
	dir, transcript, loop, teammate := continuationFixture(t, &queuedStreamProvider{})
	receipt, err := teammate.SubmitContinuation("parent", "accepted before cancellation")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunSubAgentWithContinuations(ctx, make(chan Event, 512), loop, teammate, transcript); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	saved, err := LoadSubAgentSnapshot(dir, teammate.AgentID)
	if err != nil || len(saved.PendingFollowups) != 1 || saved.PendingFollowups[0].ID != receipt.InputID {
		t.Fatalf("accepted input lost: %#v %v", saved, err)
	}
}

func TestSubAgentContinuationSyncFailureDoesNotAcknowledgeOrSteer(t *testing.T) {
	_, transcript, loop, teammate := continuationFixture(t, &queuedStreamProvider{})
	transcript.syncFile = func() error { return errors.New("fixture sync failure") }
	receipt, err := teammate.SubmitContinuation("parent", "not durably accepted")
	if err == nil || receipt.Durable || receipt.InputID != "" {
		t.Fatalf("false acknowledgement: %#v %v", receipt, err)
	}
	if steer := loop.SteerInjectDrainForTest(); steer != "" {
		t.Fatalf("failed durable input steered: %s", steer)
	}
	if len(transcript.PendingFollowups()) != 0 {
		t.Fatal("failed durable input reported accepted")
	}
}
