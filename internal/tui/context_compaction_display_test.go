package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestFooterAndSpinnerKeepCommittedUsageUntilCompactionSaved(t *testing.T) {
	m := minimalModel(200000)
	configureRewindSummary(t, m)
	for range 15 {
		m.loop.AppendUser(strings.Repeat("old context ", 100))
	}
	before := m.loop.ContextDisplaySnapshot().Used
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := m.loop.CompactNow(context.Background(), agent.CompactOptions{Force: true,
			Persist: func([]llm.Message) error { close(started); <-release; return nil }})
		done <- err
	}()
	<-started
	m.handleAgentEvent(agent.Event{Kind: agent.EventCompactionStart})
	bar := stripANSI(renderStatusBar(m))
	if !strings.Contains(bar, formatTokensRaw(before)+" tokens") || !strings.Contains(bar, "before compaction") {
		t.Errorf("footer exposed uncommitted context: %s", bar)
	}
	spinner := stripANSI(renderSpinnerStatus(m))
	if !strings.Contains(spinner, "saving compacted context") || !strings.Contains(spinner, "Auto-compaction threshold") {
		t.Errorf("spinner obscures saving stage or threshold: %s", spinner)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	m.handleAgentEvent(agent.Event{Kind: agent.EventCompactionEnd})
	bar = stripANSI(renderStatusBar(m))
	after := m.loop.ContextDisplaySnapshot().Used
	if after >= before || !strings.Contains(bar, formatTokensRaw(after)+" tokens") || strings.Contains(bar, "before compaction") {
		t.Fatalf("footer did not adopt committed context: before=%d after=%d bar=%s", before, after, bar)
	}
	if m.spinnerOverride != "" {
		t.Fatal("finished compaction kept spinner")
	}
}
