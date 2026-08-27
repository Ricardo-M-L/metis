package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// RebindProviderRuntime replaces Loop.Compactor under mu. Automatic and
// forced compaction gates must take the same lock when observing that pointer;
// otherwise model switching during a live turn is a data race even when the
// history is too short to invoke the summarizer.
func TestConcurrentProviderRebindAndCompactionGates(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	loop := &Loop{
		Provider:      p,
		Model:         "same-model",
		ContextWindow: 100_000,
		Compactor:     NewCompactor(cfg, "same-model", 100_000, p),
		Messages: []llm.Message{{
			Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "too short to summarize"}},
		}},
	}

	start := make(chan struct{})
	history := loop.History()
	historyTokens := estimateTokens(history)
	result := CompactResult{
		Applied:      true,
		BeforeTokens: historyTokens,
		AfterTokens:  historyTokens,
		History:      history,
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1_000; i++ {
			loop.RebindProviderModel(p, "same-model")
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1_000; i++ {
			loop.maybeCompactWithPressure(context.Background(), nil, 0)
			_ = loop.deferRepeatedAutoCompact(0)
			loop.noteAutoCompactPressure(result, 0, 0)
			_ = loop.tryRecoverOverflow(context.Background(), errors.New("HTTP 401 unauthorized"), nil)
			_ = loop.compactForSecondWind(context.Background(), nil)
			_ = loop.takeCompactCircuitNotice()
			_, _ = loop.PrepareSummarizeFromTurn(1)
			loop.ResetSession(history)
		}
	}()
	close(start)
	wg.Wait()
}

func TestSessionResetWinsDuringActiveCompaction(t *testing.T) {
	tests := []struct {
		name string
		want []llm.Message
		run  func(*Loop, []llm.Message)
	}{
		{
			name: "reset",
			want: nil,
			run:  func(loop *Loop, _ []llm.Message) { loop.Reset() },
		},
		{
			name: "reset-session",
			want: []llm.Message{msg(llm.RoleUser, "new session")},
			run:  func(loop *Loop, history []llm.Message) { loop.ResetSession(history) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &blockingCompactProvider{
				name: "blocking", model: "blocking-model", started: make(chan struct{}), release: make(chan struct{}),
			}
			cfg := DefaultCompactionConfig()
			cfg.MaxSummarizeInputTokens = 0
			compactor := NewCompactor(cfg, provider.model, provider.MaxContextTokens(), provider)
			compactor.LastSummary = "old session summary"
			loop := &Loop{
				Provider: provider, Model: provider.model, ContextWindow: provider.MaxContextTokens(),
				Compactor: compactor, Messages: unifiedPipelineHistory(),
			}

			compactDone := make(chan error, 1)
			go func() {
				_, err := loop.CompactNow(context.Background(), CompactOptions{Trigger: "manual", Force: true})
				compactDone <- err
			}()
			<-provider.started

			resetDone := make(chan struct{})
			go func() {
				tt.run(loop, cloneMessages(tt.want))
				close(resetDone)
			}()
			select {
			case <-resetDone:
			case <-time.After(3 * time.Second):
				t.Fatal("session reset blocked behind active compaction")
			}
			if got := loop.History(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("reset was not immediately authoritative\n got: %#v\nwant: %#v", got, tt.want)
			}

			close(provider.release)
			select {
			case err := <-compactDone:
				if err == nil || !strings.Contains(err.Error(), "history changed") {
					t.Fatalf("CompactNow error=%v, want reset/history conflict", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("active compaction did not finish")
			}

			if got := loop.History(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("history after reset\n got: %#v\nwant: %#v", got, tt.want)
			}
			if compactor.LastSummary != "" || compactor.consecutiveFailures != 0 {
				t.Fatalf("compactor state leaked across reset: summary=%q failures=%d",
					compactor.LastSummary, compactor.consecutiveFailures)
			}
		})
	}
}
