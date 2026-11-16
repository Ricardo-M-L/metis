package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestResponsesContextUpdatesPersistAcrossRealTurns(t *testing.T) {
	type request struct {
		Instructions string            `json:"instructions"`
		Input        []json.RawMessage `json:"input"`
		CacheKey     string            `json:"prompt_cache_key"`
	}
	captured := make(chan request, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		captured <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixture\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	p := openai.NewResponses("fixture-key", server.URL, "fixture-model", 256, 5*time.Second, 0)
	if err := p.ConfigureCapabilityProfile("openai"); err != nil {
		t.Fatal(err)
	}
	l := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "stable", 8)
	l.SystemSections = []llm.SystemSection{
		{Name: "base", Body: "stable", Cache: true},
		{Name: "fixture-context", Body: "before", Volatile: true},
	}
	run := func(n int) request {
		t.Helper()
		l.AppendUser(fmt.Sprintf("user turn %d", n))
		if err := l.Run(context.Background(), make(chan Event, 128)); err != nil {
			t.Fatal(err)
		}
		return <-captured
	}
	first := run(1)
	// Resume the canonical history, not a hand-built request that omits the
	// provider's preceding response. Its context baseline must be recoverable.
	l.Restore(l.History())
	second := run(2)
	if len(second.Input) < len(first.Input) || !reflect.DeepEqual(second.Input[:len(first.Input)], first.Input) {
		t.Errorf("resumed request rewrote the previous wire prefix: first=%s second=%s", first.Input, second.Input)
	}
	if n := countContextMessages(l.History()); n != 1 {
		t.Errorf("unchanged state has %d durable context updates, want one", n)
	}
	l.SystemSections[1].Body = "after"
	third := run(3)
	if len(third.Input) < len(second.Input) || !reflect.DeepEqual(third.Input[:len(second.Input)], second.Input) {
		t.Error("changed context rewrote old messages instead of appending")
	}
	if n := countContextMessages(l.History()); n != 2 {
		t.Errorf("changed state has %d durable context updates, want two", n)
	}
	l.SystemSections = l.SystemSections[:1]
	fourth := run(4)
	if fourth.Instructions != "stable" || fourth.CacheKey != first.CacheKey {
		t.Error("state changes altered stable instructions or cache routing")
	}
	if n := countContextMessages(l.History()); n != 3 {
		t.Errorf("state removal has %d updates, want three", n)
	}
	if !strings.Contains(string(fourth.Input[len(fourth.Input)-1]), "removed") || !strings.Contains(string(fourth.Input[len(fourth.Input)-1]), "fixture-context") {
		t.Error("state removal was not explicitly sent to the model")
	}
	// A legacy provider must receive its current system, not unknown control
	// messages or stale high-priority instructions from the previous route.
	fallback := &queuedStreamProvider{streams: []llm.StreamReader{textStream("done")}}
	l.RebindProviderModel(fallback, "legacy")
	l.AppendUser("continue on a compatible provider")
	if err := l.Run(context.Background(), make(chan Event, 128)); err != nil {
		t.Fatal(err)
	}
	for _, msg := range fallback.capturedRequests()[0].Messages {
		for _, block := range msg.Content {
			if block.Type == "context_update" {
				t.Fatal("unsupported provider received a context-update extension")
			}
		}
	}
}

func TestCompactionDropsDurableContextBaseline(t *testing.T) {
	p := openai.NewResponses("fixture", "http://fixture.invalid", "fixture", 256, time.Second, 0)
	if err := p.ConfigureCapabilityProfile("openai"); err != nil {
		t.Fatal(err)
	}
	l := NewLoop(p, tools.NewRegistry(), nil, nil, "base", 8)
	l.SystemSections = []llm.SystemSection{
		{Name: "base", Body: "base", Cache: true},
		{Name: "env", Body: "unchanged environment", Volatile: true},
		{Name: "plan", Body: "old plan", Volatile: true},
	}
	l.AppendUser("start")
	_, anchor := l.buildRequestWithContext(nil)
	if err := l.commitContextUpdate(anchor); err != nil {
		t.Fatal(err)
	}
	l.SystemSections[2].Body = "new plan"
	_, anchor = l.buildRequestWithContext(nil)
	if err := l.commitContextUpdate(anchor); err != nil {
		t.Fatal(err)
	}
	history := l.History()
	// A compacted keep-tail can retain only a delta, not the first full
	// snapshot. Its local State metadata must not suppress reinjection.
	candidate := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "compacted summary"}}},
		history[len(history)-1],
	}
	compacted := stripProviderState(candidate)
	if countContextMessages(compacted) != 0 {
		t.Fatal("compaction retained an incomplete context-update baseline")
	}
	l.Restore(compacted)
	req, _ := l.buildRequestWithContext(nil)
	update := req.Messages[len(req.Messages)-1].Content[0].ContextUpdate
	if update == nil || update.Revision != 1 || len(update.Changed) != 2 {
		t.Fatalf("new context window did not reestablish the full current state: %+v", update)
	}
}

func TestContextUpdateCheckpointFailureStopsBeforeProvider(t *testing.T) {
	called := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	p := openai.NewResponses("fixture", server.URL, "fixture", 256, time.Second, 0)
	if err := p.ConfigureCapabilityProfile("openai"); err != nil {
		t.Fatal(err)
	}
	l := NewLoop(p, tools.NewRegistry(), nil, nil, "base", 8)
	l.SystemSections = []llm.SystemSection{{Name: "env", Body: "fixture state", Volatile: true}}
	l.AppendUser("start")
	failure := errors.New("fixture checkpoint failure")
	l.HistoryCheckpoint = func(history []llm.Message) error {
		if countContextMessages(history) > 0 {
			return failure
		}
		return nil
	}
	if err := l.Run(context.Background(), make(chan Event, 128)); !errors.Is(err, failure) {
		t.Fatalf("Run error = %v, want checkpoint failure", err)
	}
	select {
	case <-called:
		t.Fatal("provider called before the context update was durably saved")
	default:
	}
}

func TestContextUpdateCommitPreservesRequestAnchor(t *testing.T) {
	p := openai.NewResponses("fixture", "http://fixture.invalid", "fixture", 256, time.Second, 0)
	if err := p.ConfigureCapabilityProfile("openai"); err != nil {
		t.Fatal(err)
	}
	l := NewLoop(p, tools.NewRegistry(), nil, nil, "base", 8)
	l.SystemSections = []llm.SystemSection{{Name: "env", Body: "fixture state", Volatile: true}}
	l.AppendUser("fork this context")
	req, anchor := l.buildRequestWithContext(nil)
	if err := l.commitContextUpdate(anchor); err != nil {
		t.Fatal(err)
	}
	l.rememberRequest(req, anchor)
	if l.parentSnapshot("Fork").request == nil {
		t.Fatal("an append-only context update invalidated the actual request's fork anchor")
	}
}

func TestContextPreflightCountsDurableStateOnce(t *testing.T) {
	p := &contextPreflightProvider{queuedStreamProvider: queuedStreamProvider{streams: []llm.StreamReader{textStream("done")}}}
	l := NewLoop(p, tools.NewRegistry(), nil, nil, "base", 8)
	l.SystemSections = []llm.SystemSection{{Name: "base", Body: "base", Cache: true}, {Name: "env", Body: strings.Repeat("large context ", 1000), Volatile: true}}
	l.AppendUser("start")
	if err := l.Run(context.Background(), make(chan Event, 128)); err != nil {
		t.Fatal(err)
	}
	before := l.History()
	// Only the four-character static section remains outside history:
	// four wrapper tokens plus one content token. The large state is already
	// represented by the durable update, not a second system allocation.
	want := estimateActiveHistoryTokens(before) + 5
	if got := l.EstimateRequestContextTokens(nil); got != want {
		t.Fatalf("unchanged durable state counted twice: got %d, want %d", got, want)
	}
	l.SystemSections[1].Body += " changed"
	projected := l.buildRequest(nil)
	want = estimateActiveHistoryTokens(projected.Messages) + 5
	if got := l.EstimateRequestContextTokens(nil); got != want {
		t.Fatalf("prospective delta estimate = %d, want actual projected input %d", got, want)
	}
	if !reflect.DeepEqual(l.History(), before) {
		t.Fatal("preflight persisted or rewrote a context update")
	}
}

type contextPreflightProvider struct{ queuedStreamProvider }

func (*contextPreflightProvider) ContextUpdateScope() string { return "fixture-context" }

func countContextMessages(messages []llm.Message) int {
	count := 0
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type == "context_update" {
				count++
			}
		}
	}
	return count
}
