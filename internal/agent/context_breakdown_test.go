package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestContextUsageSnapshotPreservesProviderTotalAndEstimatesParts(t *testing.T) {
	p := &activeContextTestProvider{name: "route", model: "model"}
	l := &Loop{Provider: p, Model: p.model, ContextWindow: 128000}
	primeActiveContextSnapshot(t, l, &usageTotals{in: 1000, cacheRead: 250, out: 40}, 17)
	got := l.ContextUsageSnapshot()
	if got.Used != 1290 || got.Breakdown == nil || !got.Breakdown.Estimated || got.Breakdown.SystemTokens != 17 || got.Breakdown.ToolTokens != 0 {
		t.Fatalf("provider total or request prefix changed: %+v parts=%+v", got, got.Breakdown)
	}
	if got.Breakdown.MessageTokens != estimateActiveHistoryTokens(l.History()) {
		t.Fatalf("message estimate %+v", got.Breakdown)
	}
	if got.Breakdown.SystemTokens+got.Breakdown.ToolTokens+got.Breakdown.MessageTokens == got.Used {
		t.Fatal("fixture must distinguish local category sum from provider total")
	}
	if got.Capabilities.Window != 128000 || got.Capabilities.Model != p.model {
		t.Fatalf("binding not captured with usage: %+v", got)
	}
	// The public result must not allow a caller to corrupt the cached reading.
	got.Breakdown.SystemTokens = 9999
	l.mu.Lock()
	cached := l.ContextUsageSnapshot()
	l.mu.Unlock()
	if cached.Breakdown == nil || cached.Breakdown.SystemTokens != 17 {
		t.Fatalf("mutable snapshot alias: %+v", cached.Breakdown)
	}
}

func TestContextUsageSnapshotUsesPreparedPrefixAndNeverCallsRuntime(t *testing.T) {
	calls := 0
	l := &Loop{System: strings.Repeat("ignored mirror", 1000), SystemSections: []llm.SystemSection{{Name: "base", Body: "system body"}},
		CurrentStateSections: func() []llm.SystemSection { calls++; return []llm.SystemSection{{Name: "runtime", Body: "state body"}} }, BypassNextCache: true}
	specs := []llm.ToolSpec{{Name: "Read", Description: "read a file", InputSchema: map[string]any{"type": "object"}}}
	l.AppendUser("hello")
	l.EstimateRequestContextTokens(specs)
	got := l.ContextUsageSnapshot()
	if got.Breakdown == nil || got.Breakdown.ToolTokens != estimateSpecTokens(specs[0]) || got.Breakdown.SystemTokens != 8+estimateStringTokens("system body")+estimateStringTokens("state body") {
		t.Fatalf("incorrect prepared parts: %+v", got.Breakdown)
	}
	for range 3 {
		l.ContextUsageSnapshot()
	}
	if calls != 1 || !l.BypassNextCache {
		t.Fatalf("status assembled request or consumed flag: calls=%d bypass=%v", calls, l.BypassNextCache)
	}
	l.mu.Lock()
	l.rescueNoTools = true
	l.mu.Unlock()
	l.EstimateRequestContextTokens(specs)
	got = l.ContextUsageSnapshot()
	if got.Breakdown == nil || got.Breakdown.ToolTokens != 0 || !l.rescueNoTools || !l.BypassNextCache {
		t.Fatalf("rescue parts/flags: %+v", got.Breakdown)
	}
}

func TestContextUsageSnapshotImageEstimateExcludesPayloadEncoding(t *testing.T) {
	l := &Loop{}
	l.ResetSession([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "image", Data: strings.Repeat("A", 800000)}}}})
	l.EstimateRequestContextTokens(nil)
	large := l.ContextUsageSnapshot()
	l.ResetSession([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "image", Data: "AA=="}}}})
	l.EstimateRequestContextTokens(nil)
	small := l.ContextUsageSnapshot()
	if large.Breakdown == nil || small.Breakdown == nil || large.Breakdown.MessageTokens != small.Breakdown.MessageTokens || large.Breakdown.MessageTokens > 2000 {
		t.Fatalf("base64 counted as tokens: large=%+v small=%+v", large.Breakdown, small.Breakdown)
	}
}

func TestContextUsageSnapshotInvalidatesAcrossSessionHistoryAndModel(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*Loop)
	}{
		{"session", func(l *Loop) { l.ResetSession(nil) }},
		{"history", func(l *Loop) { l.Restore(nil) }},
		{"model", func(l *Loop) { l.RebindProviderModel(&activeContextTestProvider{name: "other", model: "next"}, "next") }},
		{"identity", func(l *Loop) { l.SetPrincipalIdentity("new identity") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			l := &Loop{Provider: &activeContextTestProvider{name: "a", model: "a"}, Model: "a", System: "old prefix"}
			l.AppendUser("old history")
			l.EstimateRequestContextTokens(nil)
			if l.ContextUsageSnapshot().Breakdown == nil {
				t.Fatal("fixture missing prefix")
			}
			change.mutate(l)
			// Even if a compaction owns mu, the last epoch's complete reading cannot leak.
			l.mu.Lock()
			stale := l.ContextUsageSnapshot()
			l.mu.Unlock()
			if stale.Breakdown != nil || stale.Capabilities.Model == "a" {
				t.Fatalf("old epoch leaked under write lock: %+v", stale)
			}
			if got := l.ContextUsageSnapshot(); got.Breakdown != nil {
				t.Fatalf("old prefix reused after %s: %+v", change.name, got)
			}
		})
	}
}

func TestContextUsageSnapshotAndMetadataSyncNeverWaitForWriter(t *testing.T) {
	l := &Loop{Provider: &activeContextTestProvider{name: "a", model: "a"}, Model: "a", ContextWindow: 128000}
	l.EstimateRequestContextTokens(nil)
	want := l.ContextUsageSnapshot()
	l.mu.Lock()
	done := make(chan ContextUsage, 1)
	go func() { l.TrySyncContextCapabilities(); done <- l.ContextUsageSnapshot() }()
	select {
	case got := <-done:
		if got.Used != want.Used || got.Capabilities != want.Capabilities || got.Breakdown == nil {
			t.Fatalf("incoherent cached reading: %+v", got)
		}
	case <-time.After(500 * time.Millisecond):
		l.mu.Unlock()
		t.Fatal("status blocked on request/history lock")
	}
	l.mu.Unlock()
}

func TestContextPreflightDoesNotPublishAcrossCallbackSessionSwitch(t *testing.T) {
	for _, changeModel := range []bool{false, true} {
		t.Run(map[bool]string{false: "history", true: "model"}[changeModel], func(t *testing.T) {
			l := &Loop{Provider: &activeContextTestProvider{name: "a", model: "a"}, Model: "a", System: "old prefix"}
			l.AppendUser("old history")
			l.CurrentStateSections = func() []llm.SystemSection {
				if changeModel {
					l.RebindProviderModel(&activeContextTestProvider{name: "b", model: "b"}, "b")
				} else {
					l.ResetSession(nil)
				}
				return []llm.SystemSection{{Name: "old runtime", Body: strings.Repeat("stale ", 100)}}
			}
			l.EstimateRequestContextTokens(nil)
			got := l.ContextUsageSnapshot()
			if got.Breakdown != nil || l.requestOverheadTokens.Load() != 0 {
				t.Fatalf("stale preflight published after replacement: %+v overhead=%d", got, l.requestOverheadTokens.Load())
			}
		})
	}
}

func TestContextPreflightDoesNotPublishAcrossPrincipalChange(t *testing.T) {
	l := &Loop{System: "system"}
	l.CurrentStateSections = func() []llm.SystemSection {
		l.SetPrincipalIdentity("new principal")
		return []llm.SystemSection{{Name: "old runtime", Body: "old principal state"}}
	}
	l.EstimateRequestContextTokens(nil)
	if got := l.ContextUsageSnapshot(); got.Breakdown != nil || l.requestOverheadTokens.Load() != 0 {
		t.Fatalf("old principal prefix published: %+v", got)
	}
}

func TestContextUsageSnapshotConcurrentRoutingNeverMixesCategories(t *testing.T) {
	a := &activeContextTestProvider{name: "route-a", model: "a"}
	b := &activeContextTestProvider{name: "route-b", model: "b"}
	l := &Loop{}
	l.RebindProviderRuntime(a, "a", 0, "a system", nil)
	done, failed := make(chan struct{}), make(chan string, 1)
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			p, body := a, "a system"
			if i%2 != 0 {
				p, body = b, "b system with different length"
			}
			l.RebindProviderRuntime(p, p.model, 0, body, nil)
			l.ResetSession(nil)
			l.AppendUser("current conversation")
			l.EstimateRequestContextTokens(nil)
		}
	}()
	for {
		got := l.ContextUsageSnapshot()
		if parts := got.Breakdown; parts != nil {
			body := "a system"
			if got.Capabilities.Model == "b" {
				body = "b system with different length"
			} else if got.Capabilities.Model != "a" {
				t.Fatalf("unowned category model: %+v", got)
			}
			if parts.SystemTokens != estimateStringTokens(body) || parts.ToolTokens != 0 || got.Capabilities.Window != 128000 {
				select {
				case failed <- "mixed model/window/prefix":
				default:
				}
			}
		}
		select {
		case <-done:
			select {
			case reason := <-failed:
				t.Fatal(reason)
			default:
			}
			return
		default:
		}
	}
}

func TestContextBreakdownSurvivesUnusableProviderUsage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage usageTotals
		body  string
	}{
		{name: "missing prompt", usage: usageTotals{out: 4}, body: "question"},
		{name: "underreported prompt", usage: usageTotals{in: 1, out: 4}, body: strings.Repeat("question ", 1000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &activeContextTestProvider{name: "route", model: "model"}
			l := &Loop{Provider: p, Model: p.model, ContextWindow: 128000}
			l.AppendUser(tc.body)
			l.mu.Lock()
			req := llm.Request{Model: p.model, System: "valid system prefix", Messages: append([]llm.Message(nil), l.Messages...)}
			anchor := l.contextRequestAnchorLocked(p, req)
			l.mu.Unlock()
			before := l.ContextUsageSnapshot()
			if before.Breakdown == nil {
				t.Fatal("fixture prefix not prepared")
			}
			l.mu.Lock()
			l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "answer"}}})
			l.storeActiveContextSnapshotLocked(&tc.usage, anchor)
			l.mu.Unlock()
			got := l.ContextUsageSnapshot()
			want := estimateActiveHistoryTokens(l.History()) + estimateRequestOverhead(req)
			if got.Used != want || got.Breakdown == nil || got.Breakdown.SystemTokens != before.Breakdown.SystemTokens || got.Breakdown.MessageTokens != estimateActiveHistoryTokens(l.History()) {
				t.Fatalf("unusable provider usage lost known local categories: got=%+v parts=%+v want total=%d", got, got.Breakdown, want)
			}
			l.mu.Lock()
			cached := l.ContextUsageSnapshot()
			l.mu.Unlock()
			if cached.Used != got.Used || cached.Breakdown == nil {
				t.Fatalf("stale cached provider total: %+v", cached)
			}
		})
	}
}

func TestContextBreakdownOldResponseCannotClearNewScopePrefix(t *testing.T) {
	old := &activeContextTestProvider{name: "old-route", model: "old"}
	l := &Loop{Provider: old, Model: old.model, ContextWindow: 128000}
	l.AppendUser("old question")
	l.mu.Lock()
	anchor := l.contextRequestAnchorLocked(old, llm.Request{Model: old.model, System: "old prefix", Messages: append([]llm.Message(nil), l.Messages...)})
	l.mu.Unlock()
	next := &activeContextTestProvider{name: "new-route", model: "new"}
	l.RebindProviderRuntime(next, next.model, 0, "new complete prefix", nil)
	l.ResetSession([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "new question"}}}})
	l.EstimateRequestContextTokens(nil)
	before := l.ContextUsageSnapshot()
	if before.Breakdown == nil {
		t.Fatal("new scope missing prepared prefix")
	}
	l.mu.Lock()
	l.storeActiveContextSnapshotLocked(&usageTotals{out: 4}, anchor)
	l.mu.Unlock()
	after := l.ContextUsageSnapshot()
	if after.Breakdown == nil || *after.Breakdown != *before.Breakdown || after.Used != before.Used || after.Capabilities != before.Capabilities || after.epoch != before.epoch {
		t.Fatalf("old response changed new scope: before=%+v parts=%+v after=%+v parts=%+v", before, before.Breakdown, after, after.Breakdown)
	}
}
