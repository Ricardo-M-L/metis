package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestContextUpdateSourceDeletionErasesResumeMetadataAndWarmHeader(t *testing.T) {
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const fact = "Nebula private customer endpoint is secret-nebula.invalid."
	if err := mm.Archival().Insert(memory.Passage{ID: "private", Content: fact, SourceSessionID: "source"}); err != nil {
		t.Fatal(err)
	}
	p := openai.NewResponses("fixture", "http://fixture.invalid", "fixture", 256, time.Second, 0)
	if err := p.ConfigureCapabilityProfile("openai"); err != nil {
		t.Fatal(err)
	}
	parent := NewLoop(p, tools.NewRegistry(), nil, nil, "stable", 8)
	parent.Memory = mm
	parent.AutoRetrieveK = 1
	parent.SystemSections = []llm.SystemSection{{Name: "base", Body: "stable", Cache: true}}
	parent.AppendUser("Nebula customer endpoint")
	req, anchor := parent.buildRequestWithContext(nil)
	encoded, _ := json.Marshal(req)
	if !strings.Contains(string(encoded), fact) {
		t.Fatal("fixture did not expose source memory")
	}
	if err := parent.commitContextUpdate(anchor); err != nil {
		t.Fatal(err)
	}
	parent.rememberRequest(req, anchor)
	snap := parent.parentSnapshot("Fork")
	child := NewLoop(p, tools.NewRegistry(), nil, nil, "", 8)
	child.RestoreFork(snap, snap.Messages)
	if err := mm.DeleteSession("source"); err != nil {
		t.Fatal(err)
	}
	resumed := child.buildRequest(nil)
	for name, value := range map[string]any{"request": resumed, "durable": child.History()} {
		encoded, _ := json.Marshal(value)
		if strings.Contains(string(encoded), fact) {
			t.Fatalf("revoked source survives in %s, including full State metadata", name)
		}
	}
	if child.warmRequest != nil {
		t.Fatal("revoked memory left a reusable warm header")
	}
	if child.memoryRevocationBefore == nil {
		t.Fatal("revocation did not require a full durable replacement")
	}
	// A fresh resume from the sanitized metadata must not recover the old source.
	again := NewLoop(p, tools.NewRegistry(), nil, nil, "stable", 8)
	again.Memory = mm
	again.SystemSections = parent.SystemSections
	again.Restore(child.History())
	encoded, _ = json.Marshal(again.buildRequest(nil))
	if strings.Contains(string(encoded), fact) {
		t.Fatal("resume resurrected a revoked source")
	}
}

func TestContextUpdateMetadataIsDetachedAtHistoryBoundary(t *testing.T) {
	p := &contextPreflightProvider{}
	l := NewLoop(p, tools.NewRegistry(), nil, nil, "base", 8)
	l.SystemSections = []llm.SystemSection{{Name: "env", Body: "original", Volatile: true}}
	l.AppendUser("start")
	req, anchor := l.buildRequestWithContext(nil)
	if err := l.commitContextUpdate(anchor); err != nil {
		t.Fatal(err)
	}
	req.Messages[len(req.Messages)-1].Content[0].ContextUpdate.State[0].Body = "request mutation"
	snapshot := l.History()
	if snapshot[len(snapshot)-1].Content[0].ContextUpdate.State[0].Body != "original" {
		t.Fatal("committed metadata aliases request")
	}
	snapshot[len(snapshot)-1].Content[0].ContextUpdate.State[0].Body = "history reader mutation"
	if l.History()[len(snapshot)-1].Content[0].ContextUpdate.State[0].Body != "original" {
		t.Fatal("history snapshot aliases canonical metadata")
	}
}

func TestContextUpdateCapabilitySurvivesExecutionBinding(t *testing.T) {
	p := openai.NewCodexResponses("fixture", 0, time.Second, 0, nil)
	wrapped := llm.BindExecutionProvider(llm.BindExecutionProvider(p, context.Background()), context.Background())
	if got := wrapped.(provider.ContextUpdateProvider).ContextUpdateScope(); got != "responses-local-v1" {
		t.Fatalf("scope lost: %q", got)
	}
	p.StateMode = openai.ResponsesStateProvider
	// Codex remains local by contract; a normal native provider follows its live mode.
	normal := openai.NewResponses("fixture", "https://api.openai.com/v1", "fixture", 0, time.Second, 0)
	bound := llm.BindExecutionProvider(normal, context.Background())
	normal.StateMode = openai.ResponsesStateProvider
	if got := bound.(provider.ContextUpdateProvider).ContextUpdateScope(); got != "" {
		t.Fatalf("managed route acquired ordered context: %q", got)
	}
}

func TestWarmForkPermissionChangeDropsStaleHeader(t *testing.T) {
	p := &queuedStreamProvider{}
	gate := permission.New(permission.ModeBypass)
	parent := NewLoop(p, tools.NewRegistry(), gate, nil, "base", 8)
	parent.CurrentStateSnapshot = func() RuntimeStateSnapshot { return RuntimeStateSnapshot{PermissionMode: string(gate.Mode())} }
	parent.SystemSections = []llm.SystemSection{{Name: "base", Body: "base"}}
	parent.AppendUser("task")
	req, anchor := parent.buildRequestWithContext(nil)
	parent.rememberRequest(req, anchor)
	snap := parent.parentSnapshot("Fork")
	child := NewLoop(p, tools.NewRegistry(), gate, nil, "", 8)
	child.RestoreFork(snap, snap.Messages)
	gate.SetMode(permission.ModePlan)
	actual := child.buildRequest(nil)
	for _, section := range actual.SystemSections {
		if section.Name == "runtime_state" && !strings.Contains(section.Body, "permission_mode: plan") {
			t.Fatal("warm request overrode current child gate")
		}
	}
	if child.warmRequest != nil {
		t.Fatal("permission replacement retained stale pin")
	}
}

func TestContextUpdateAdmissionCountsPendingDeltaWithProviderUsage(t *testing.T) {
	p := &contextPreflightProvider{}
	l := NewLoop(p, tools.NewRegistry(), nil, nil, "base", 8)
	l.ContextWindow = 1000
	l.SystemSections = []llm.SystemSection{{Name: "base", Body: "base", Cache: true}, {Name: "env", Body: strings.Repeat("new state ", 1000), Volatile: true}}
	l.AppendUser("start")
	l.activeContext = activeContextSnapshot{valid: true, tokens: 500, messageCount: len(l.Messages), historyRevision: l.historyRevision, routingRevision: l.routingRevision, requestOverhead: 5, provider: identifyContextProvider(p), requestModel: l.Model}
	base, ok := l.activeContextBaseLocked()
	if !ok {
		t.Fatal("fixture usage anchor invalid")
	}
	req, anchor := l.buildRequestWithContext(nil)
	if anchor.contextUpdate == nil {
		t.Fatal("fixture lacks pending context delta")
	}
	delta := estimateActiveHistoryTokens([]llm.Message{*anchor.contextUpdate})
	want := base + anchor.requestOverhead + delta
	if anchor.requestContextTokens != want {
		t.Fatalf("admission omitted pending state: got %d want %d", anchor.requestContextTokens, want)
	}
	if got := l.EstimateContextTokens(); got != want {
		t.Fatalf("pending meter=%d want %d", got, want)
	}
	if _, _, err := l.admitBuiltRequest(context.Background(), nil, nil, 0, req, anchor); err == nil {
		t.Fatal("oversized post-preflight state bypassed hard admission")
	}
	if err := l.commitContextUpdate(anchor); err != nil {
		t.Fatal(err)
	}
	if got := l.EstimateContextTokens(); got != want {
		t.Fatalf("commit double counted pending state: got %d want %d", got, want)
	}
}

func TestWarmForkExplicitPlanChangeDropsParentPin(t *testing.T) {
	p := &queuedStreamProvider{}
	parent := NewLoop(p, tools.NewRegistry(), nil, nil, "base", 8)
	parent.SystemSections = []llm.SystemSection{{Name: "base", Body: "base"}}
	parent.AppendUser("task")
	req, anchor := parent.buildRequestWithContext(nil)
	parent.rememberRequest(req, anchor)
	snap := parent.parentSnapshot("Fork")
	child := NewLoop(p, tools.NewRegistry(), nil, nil, "", 8)
	child.RestoreFork(snap, snap.Messages)
	child.SetPlanMode(true)
	actual := child.buildRequest(nil)
	if child.warmRequest != nil {
		t.Fatal("explicit planning change retained warm pin")
	}
	found := false
	for _, section := range actual.SystemSections {
		if section.Name == "plan_mode" {
			found = true
		}
	}
	if !found {
		t.Fatal("current plan overlay was replaced by inherited request")
	}
}
