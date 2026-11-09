package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func displayCompactionLoop(provider llm.Provider) *Loop {
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 0
	l := &Loop{Provider: provider, Model: "test", ContextWindow: 128000,
		Compactor: NewCompactor(cfg, "test", 128000, provider), Messages: unifiedPipelineHistory()}
	l.EstimateRequestContextTokens(nil)
	l.mu.Lock()
	l.activeContext = activeContextSnapshot{valid: true, tokens: 40000, messageCount: len(l.Messages),
		historyRevision: l.historyRevision, routingRevision: l.routingRevision,
		requestOverhead: int(l.requestOverheadTokens.Load()), provider: identifyContextProvider(provider), requestModel: l.Model}
	l.mu.Unlock()
	return l
}

func TestContextDisplayFreezesUntilDurableCompactionCommit(t *testing.T) {
	provider := &blockingCompactProvider{name: "blocking", model: "test", started: make(chan struct{}), release: make(chan struct{})}
	l := displayCompactionLoop(provider)
	before := l.ContextDisplaySnapshot()
	persistStarted, persistRelease := make(chan struct{}), make(chan struct{})
	events := make(chan Event, 32)
	done := make(chan error, 1)
	go func() {
		_, err := l.CompactNow(context.Background(), CompactOptions{Force: true,
			Emit: func(ev Event) { events <- ev },
			Persist: func([]llm.Message) error {
				l.EstimateRequestContextTokens(nil)
				close(persistStarted)
				<-persistRelease
				return nil
			}})
		done <- err
	}()
	<-provider.started
	if got := l.ContextDisplaySnapshot(); got.Used != before.Used || got.CompactionStage != "summarizing" {
		t.Errorf("summary display=%+v, want old usage=%d and summarizing", got, before.Used)
	}
	close(provider.release)
	<-persistStarted
	if actual := l.EstimateContextTokens(); actual >= before.Used {
		t.Error("internal hook/budget view must observe the smaller candidate")
	}
	if got := l.ContextDisplaySnapshot(); got.Used != before.Used || got.CompactionStage != "saving" {
		t.Errorf("pending persistence exposed candidate: %+v, before=%d", got, before.Used)
	}
	l.mu.Lock()
	read := make(chan ContextUsage, 1)
	go func() { read <- l.ContextDisplaySnapshot() }()
	select {
	case got := <-read:
		if got.Used != before.Used || got.CompactionStage != "saving" {
			t.Errorf("busy display=%+v", got)
		}
	case <-time.After(time.Second):
		t.Error("display blocks on Loop writer")
	}
	l.mu.Unlock()
	close(persistRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := l.ContextDisplaySnapshot(); got.CompactionStage != "" || got.Used != l.EstimateContextTokens() || got.Used >= before.Used {
		t.Fatalf("committed display=%+v, actual=%d", got, l.EstimateContextTokens())
	}
	close(events)
	found := false
	for ev := range events {
		if ev.Kind == EventContextCompacted {
			found = true
			if ev.PreviousContextTokens != before.Used || ev.ContextTokens != l.DisplayContextTokens() {
				t.Errorf("success metrics disagree with display: %+v", ev)
			}
		}
	}
	if !found {
		t.Error("committed compaction omitted success event")
	}
}

func TestContextDisplayPostHookUsesCandidateButDisplayKeepsCommittedUsage(t *testing.T) {
	l := displayCompactionLoop(&fakeSummarizer{})
	before := l.ContextDisplaySnapshot()
	hooks := NewHookRegistry()
	hooks.Register(PostCompactHandler(func(context.Context, HookContext, *PostCompact) *ModifiedPostCompact {
		l.EstimateRequestContextTokens(nil)
		if got := l.ContextDisplaySnapshot(); got.Used != before.Used || got.CompactionStage != "checking" {
			t.Errorf("post-hook display=%+v", got)
		}
		if len(l.History()) >= len(unifiedPipelineHistory()) {
			t.Error("hook lost historical candidate semantics")
		}
		return &ModifiedPostCompact{AdditionalContext: strings.Repeat("oversized hook context ", 50000)}
	}))
	l.Hooks = hooks
	_, err := l.CompactNow(context.Background(), CompactOptions{Force: true})
	if err == nil {
		t.Fatal("oversized hook must reject candidate")
	}
	if got := l.ContextDisplaySnapshot(); got.Used != before.Used || got.CompactionStage != "" {
		t.Fatalf("hook rollback lost provider usage: %+v want=%d", got, before.Used)
	}
}

func TestContextDisplayPersistFailureRestoresProviderUsageAndConcurrentSuffix(t *testing.T) {
	l := displayCompactionLoop(&fakeSummarizer{})
	before := l.ContextDisplaySnapshot()
	const suffix = "new note during saving"
	_, err := l.CompactNow(context.Background(), CompactOptions{Force: true, Persist: func([]llm.Message) error {
		l.AppendUser(suffix)
		return errors.New("disk full")
	}})
	if err == nil {
		t.Fatal("expected persistence failure")
	}
	want := before.Used + estimateActiveHistoryTokens([]llm.Message{msg(llm.RoleUser, suffix)})
	if got := l.ContextDisplaySnapshot(); got.Used != want || got.CompactionStage != "" {
		t.Fatalf("failed commit display=%+v want=%d", got, want)
	}
}

func TestContextDisplayDoesNotLeakCompactionAcrossSessionOrRouteChanges(t *testing.T) {
	for _, change := range []struct {
		name string
		run  func(*Loop)
	}{
		{"session", func(l *Loop) { l.ResetSession([]llm.Message{msg(llm.RoleUser, "new session")}) }},
		{"same-history-new-session", func(l *Loop) { l.ResetSession(l.History()) }},
		{"identity", func(l *Loop) { l.SetPrincipalIdentity("new principal") }},
		{"route", func(l *Loop) { l.RebindProviderModel(&fakeSummarizer{}, "different-model") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			provider := &blockingCompactProvider{name: "blocking", model: "test", started: make(chan struct{}), release: make(chan struct{})}
			l := displayCompactionLoop(provider)
			done := make(chan error, 1)
			go func() { _, err := l.CompactNow(context.Background(), CompactOptions{Force: true}); done <- err }()
			<-provider.started
			change.run(l)
			l.EstimateRequestContextTokens(nil)
			if got := l.ContextDisplaySnapshot(); got.CompactionStage != "" || got.Used != l.EstimateContextTokens() {
				t.Errorf("old transaction leaked: %+v", got)
			}
			close(provider.release)
			if err := <-done; err == nil && change.name != "identity" {
				t.Fatal("changed history/route accepted old summary")
			}
			if got := l.ContextDisplaySnapshot(); got.CompactionStage != "" {
				t.Errorf("finished transaction leaked: %+v", got)
			}
		})
	}
}
