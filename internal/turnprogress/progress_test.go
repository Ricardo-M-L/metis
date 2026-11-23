package turnprogress

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestObserverUsesFixedPhasesAndKeepsZero(t *testing.T) {
	var got []Update
	ctx := WithObserver(context.Background(), func(update Update) { got = append(got, update) })
	if !HasObserver(ctx) || HasObserver(context.Background()) {
		t.Fatal("observer context ownership")
	}
	Emit(ctx, PhaseRequestPrepare)
	Emit(ctx, PhaseRequestPrepare) // a repeated boundary is not another transition
	Observe(ctx, Update{Phase: Phase("private prompt https://secret.invalid"), ElapsedMS: ms(0)})
	Emit(ctx, PhaseThinking)
	if len(got) != 2 || got[0].Phase != PhaseRequestPrepare || got[1].Phase != PhaseThinking || got[0].ElapsedMS == nil {
		t.Fatalf("updates: %+v", got)
	}
	raw, err := json.Marshal(Snapshot{RequestID: "r", Sequence: 1, Phase: PhaseReceived, ServerElapsedMS: ms(0), PhaseElapsedMS: ms(0)})
	if err != nil || !strings.Contains(string(raw), `"serverElapsedMs":0`) || !strings.Contains(string(raw), `"phaseElapsedMs":0`) || strings.Contains(string(raw), `"elapsedMs"`) {
		t.Fatalf("zero/unknown snapshot: %s, %v", raw, err)
	}
}

func TestObserverFailureDoesNotChangeRequest(t *testing.T) {
	ctx := WithObserver(context.Background(), func(Update) { panic("broken progress observer") })
	Emit(ctx, PhaseThinking)
	Emit(nil, PhaseThinking)
	Emit(context.Background(), PhaseThinking)
}

func TestFinalizingIsObservedBeforeTerminalCompletion(t *testing.T) {
	if !Phase("finalizing").Valid() || Phase("finalizing").Terminal() {
		t.Fatal("saving results must be a valid nonterminal phase")
	}
	var got []Update
	ctx := WithObserver(context.Background(), func(update Update) { got = append(got, update) })
	Emit(ctx, Phase("finalizing"))
	Emit(ctx, PhaseCompleted)
	if len(got) != 2 || got[0].Phase != Phase("finalizing") || got[1].Phase != PhaseCompleted {
		t.Fatalf("finalization lost its separate boundary: %+v", got)
	}
}

func TestSummaryMergePreservesKnownMilestonesAndUnknowns(t *testing.T) {
	current := &StartupSummary{RequestID: "r", ReceivedMS: ms(0), FirstThinkingMS: ms(5)}
	merged := MergeStartupSummary(current, &StartupSummary{RequestID: "r", FirstTextMS: ms(8), TotalMS: ms(10)})
	if merged == current || merged.ReceivedMS == current.ReceivedMS || *merged.ReceivedMS != 0 || *merged.FirstThinkingMS != 5 || *merged.FirstTextMS != 8 || merged.FirstByteMS != nil {
		t.Fatalf("merged: %+v", merged)
	}
	if replaced := MergeStartupSummary(current, &StartupSummary{RequestID: "new", FirstTextMS: ms(2)}); replaced.ReceivedMS != nil || replaced.RequestID != "new" {
		t.Fatalf("cross-request merge: %+v", replaced)
	}
}

func TestReadyMilestonesAndSummaryClockSource(t *testing.T) {
	for _, milestone := range []Milestone{MilestoneWorkerStarted, MilestoneRuntimeReady, MilestoneMCPReady} {
		if !(Update{Phase: PhaseRuntimePrepare, Milestone: milestone}).Valid() {
			t.Fatalf("invalid known milestone %q", milestone)
		}
	}
	current := &StartupSummary{Source: SourceServerObserved, ExecutionQueueMS: ms(0)}
	merged := MergeStartupSummary(current, &StartupSummary{TotalMS: ms(2)})
	if merged.Source != SourceServerObserved || merged.ExecutionQueueMS == nil || *merged.ExecutionQueueMS != 0 {
		t.Fatalf("lost summary clock source or observed zero: %+v", merged)
	}
}

func ms(value int64) *int64 { return &value }
