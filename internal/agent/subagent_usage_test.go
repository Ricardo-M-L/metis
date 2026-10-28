package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	pubsess "github.com/Ricardo-M-L/metis/pkg/session"
)

type subAgentUsageTestStream struct {
	events []llm.StreamEvent
	index  int
	endErr error
}

func (s *subAgentUsageTestStream) Recv() (llm.StreamEvent, error) {
	if s.index >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	if s.index == len(s.events) {
		return event, s.endErr
	}
	return event, nil
}

func (*subAgentUsageTestStream) Close() error { return nil }

func newSubAgentUsageTestRecorder(t *testing.T) (string, *SubAgentUsageRecorder) {
	t.Helper()
	dir := t.TempDir()
	header := NewSubAgentHeader("agt-usage", "test-model", "parent-a", "", "", "")
	transcript, err := NewSubAgentTranscript(dir, header.ID, header)
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	recorder, err := NewSubAgentUsageRecorder(dir, header.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	return dir, recorder
}

func consumeSubAgentUsageTestStream(t *testing.T, recorder *SubAgentUsageRecorder, events ...llm.StreamEvent) {
	t.Helper()
	stream := observeSubAgentUsageStream(WithSubAgentUsageRecorder(context.Background(), recorder), &subAgentUsageTestStream{events: events})
	for {
		_, err := stream.Recv()
		if err != nil {
			break
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSubAgentUsageAbsoluteReceiptsAndZeroPresence(t *testing.T) {
	dir, recorder := newSubAgentUsageTestRecorder(t)
	usage, err := LoadSubAgentUsage(dir, "agt-usage")
	if err != nil || usage.Available {
		t.Fatalf("unreported usage = %+v, error %v", usage, err)
	}
	consumeSubAgentUsageTestStream(t, recorder, llm.StreamEvent{Type: "text_delta", TextDelta: "content is not token evidence"})
	usage, err = LoadSubAgentUsage(dir, "agt-usage")
	if err != nil || usage.Available {
		t.Fatalf("content was estimated as usage: %+v, %v", usage, err)
	}
	consumeSubAgentUsageTestStream(t, recorder,
		llm.StreamEvent{Type: "usage", InputTokens: 9, OutputTokens: 7, CacheReadInputTokens: 6, CacheCreationInputTokens: 5},
		llm.StreamEvent{Type: "usage", InputTokens: 9, OutputTokens: 7, CacheReadInputTokens: 6, CacheCreationInputTokens: 5},
		llm.StreamEvent{Type: "usage", InputTokens: 4, HasInputTokens: true, OutputTokens: 0, HasOutputTokens: true},
	)
	usage, err = LoadSubAgentUsage(dir, "agt-usage")
	if err != nil || !usage.Available || usage.InputTokens != 4 || usage.OutputTokens != 0 || usage.CacheReadInputTokens != 6 || usage.CacheCreationInputTokens != 5 {
		t.Fatalf("absolute receipt duplicated or correction lost: %+v, %v", usage, err)
	}
	stable, err := LoadSubAgentUsage(dir, "agt-usage")
	if err != nil || stable != usage {
		t.Fatalf("unchanged cached result = %+v, want %+v, error %v", stable, usage, err)
	}
	zeroDir, zero := newSubAgentUsageTestRecorder(t)
	consumeSubAgentUsageTestStream(t, zero, llm.StreamEvent{HasInputTokens: true, HasOutputTokens: true})
	usage, err = LoadSubAgentUsage(zeroDir, "agt-usage")
	if err != nil || !usage.Available || usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Fatalf("reported zero must be available: %+v, %v", usage, err)
	}
}

func TestSubAgentUsageFinalValueWithErrorAndTerminalIsIdempotent(t *testing.T) {
	dir, recorder := newSubAgentUsageTestRecorder(t)
	providerErr := errors.New("provider interrupted")
	base := &subAgentUsageTestStream{events: []llm.StreamEvent{{Type: "usage", HasInputTokens: true, InputTokens: 11}}, endErr: providerErr}
	stream := observeSubAgentUsageStream(WithSubAgentUsageRecorder(context.Background(), recorder), base)
	event, err := stream.Recv()
	if err != providerErr || event.InputTokens != 11 {
		t.Fatalf("receipt wrapper changed provider result: %+v, %v", event, err)
	}
	_ = stream.Close()
	_ = stream.Close()
	finishSubAgentUsageStream(stream)
	usage, err := LoadSubAgentUsage(dir, "agt-usage")
	if err != nil || usage.InputTokens != 11 {
		t.Fatalf("final receipt lost or double counted: %+v, %v", usage, err)
	}
	// A recovered stream is a separate billable attempt, even when its draft
	// replaces the first attempt's text in the transcript.
	consumeSubAgentUsageTestStream(t, recorder, llm.StreamEvent{InputTokens: 3, OutputTokens: 2})
	usage, err = LoadSubAgentUsage(dir, "agt-usage")
	if err != nil || usage.InputTokens != 14 || usage.OutputTokens != 2 {
		t.Fatalf("separate attempts were merged: %+v, %v", usage, err)
	}
}

func TestSubAgentUsageRejectsNegativeFrameAndShadowsParent(t *testing.T) {
	dir, recorder := newSubAgentUsageTestRecorder(t)
	consumeSubAgentUsageTestStream(t, recorder,
		llm.StreamEvent{InputTokens: 10, OutputTokens: 6},
		llm.StreamEvent{InputTokens: -1, HasInputTokens: true, OutputTokens: 0, HasOutputTokens: true},
	)
	usage, err := LoadSubAgentUsage(dir, "agt-usage")
	if err != nil || usage.InputTokens != 10 || usage.OutputTokens != 6 {
		t.Fatalf("invalid frame erased prior receipt: %+v, %v", usage, err)
	}
	parentCtx := WithSubAgentUsageRecorder(context.Background(), recorder)
	childCtx := WithSubAgentUsageRecorder(parentCtx, nil)
	base := &subAgentUsageTestStream{events: []llm.StreamEvent{{InputTokens: 100}}}
	if wrapped := observeSubAgentUsageStream(childCtx, base); wrapped != base {
		t.Fatal("child without a transcript inherited the parent's usage recorder")
	}
}

func TestSubAgentUsageConcurrentAppendAndCrashTailRepair(t *testing.T) {
	dir, recorder := newSubAgentUsageTestRecorder(t)
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			consumeSubAgentUsageTestStream(t, recorder, llm.StreamEvent{InputTokens: 1, HasInputTokens: true})
		}()
	}
	group.Wait()
	usage, err := LoadSubAgentUsage(dir, "agt-usage")
	if err != nil || usage.InputTokens != 20 {
		t.Fatalf("concurrent receipts lost: %+v, %v", usage, err)
	}
	f, err := os.OpenFile(recorder.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"version":1,"receipt_id":"crash`)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	withTail, err := LoadSubAgentUsage(dir, "agt-usage")
	if err != nil || withTail.InputTokens != 20 {
		t.Fatalf("crash tail destroyed valid receipts: %+v, %v", withTail, err)
	}
	consumeSubAgentUsageTestStream(t, recorder, llm.StreamEvent{InputTokens: 2})
	repaired, err := LoadSubAgentUsage(dir, "agt-usage")
	if err != nil || repaired.InputTokens != 22 || repaired.Revision == withTail.Revision {
		t.Fatalf("append after crash failed: %+v, %v", repaired, err)
	}
}

func TestSubAgentUsageCurrentHeaderRejectsLateOldProducer(t *testing.T) {
	dir, old := newSubAgentUsageTestRecorder(t)
	consumeSubAgentUsageTestStream(t, old, llm.StreamEvent{InputTokens: 10})
	header := NewSubAgentHeader("agt-usage", "test-model", "parent-b", "", "", "")
	transcript, err := NewSubAgentTranscript(dir, "agt-usage", header)
	if err != nil {
		t.Fatal(err)
	}
	_ = transcript.Close()
	consumeSubAgentUsageTestStream(t, old, llm.StreamEvent{InputTokens: 999})
	usage, err := LoadSubAgentUsage(dir, "agt-usage")
	if err != nil || usage.Available || usage.ParentSessionID != "parent-b" {
		t.Fatalf("old producer leaked to replacement header: %+v, %v", usage, err)
	}
}

func TestSubAgentUsageSameHeaderResumeAndWriteFailureAreAdvisory(t *testing.T) {
	dir, recorder := newSubAgentUsageTestRecorder(t)
	consumeSubAgentUsageTestStream(t, recorder, llm.StreamEvent{InputTokens: 10})
	header, err := LoadSubAgentHeader(dir, "agt-usage")
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := NewSubAgentUsageRecorder(dir, "agt-usage", &header)
	if err != nil {
		t.Fatal(err)
	}
	consumeSubAgentUsageTestStream(t, resumed, llm.StreamEvent{InputTokens: 2})
	usage, err := LoadSubAgentUsage(dir, "agt-usage")
	if err != nil || usage.InputTokens != 12 {
		t.Fatalf("durable same-header resume = %+v, %v", usage, err)
	}
	blockedPath := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blockedPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	broken := &SubAgentUsageRecorder{path: filepath.Join(blockedPath, "usage.jsonl"), owner: recorder.owner}
	base := &subAgentUsageTestStream{events: []llm.StreamEvent{{InputTokens: 7}}, endErr: io.EOF}
	stream := observeSubAgentUsageStream(WithSubAgentUsageRecorder(context.Background(), broken), base)
	event, readErr := stream.Recv()
	if event.InputTokens != 7 || readErr != io.EOF || stream.Close() != nil {
		t.Fatalf("display persistence failure altered provider result: %+v, %v", event, readErr)
	}
}

func TestSubAgentUsageRejectsUnsafeIDs(t *testing.T) {
	for _, id := range []string{"", "../outside", "a/b", "a\\b"} {
		if _, err := LoadSubAgentUsage(t.TempDir(), id); err == nil {
			t.Errorf("Load accepted unsafe ID %q", id)
		}
		if _, err := NewSubAgentUsageRecorder(t.TempDir(), id, &pubsess.Header{CreatedAt: time.Now()}); err == nil {
			t.Errorf("New accepted unsafe ID %q", id)
		}
	}
}
