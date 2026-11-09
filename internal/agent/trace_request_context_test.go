package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

type traceContextFixtureProvider struct{ received llm.Request }

func (*traceContextFixtureProvider) Name() string          { return "fixture" }
func (*traceContextFixtureProvider) ModelID() string       { return "selected-model" }
func (*traceContextFixtureProvider) MaxContextTokens() int { return 1000 }
func (*traceContextFixtureProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, nil
}
func (p *traceContextFixtureProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	p.received = req
	return &recoveryTestStream{}, nil
}

func TestTraceRequestContextObservesAssembledRequestWithoutChangingIt(t *testing.T) {
	traceHookMu.RLock()
	previous := traceHook
	traceHookMu.RUnlock()
	defer SetTraceHook(previous)
	var captured []Event
	SetTraceHook(func(ev Event) { captured = append(captured, ev) })
	p := &traceContextFixtureProvider{}
	req := llm.Request{System: "final logical system", SystemSections: []llm.SystemSection{{Name: "actual section", Body: "final logical system", Volatile: true}},
		Tools: []llm.ToolSpec{{Name: "Read", InputSchema: map[string]any{"type": "object"}}}, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "image", Data: "private attachment payload"}}}}}
	ctx := WithTraceInvocationID(context.Background(), "owner")
	s, err := timedProviderStream(ctx, p, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if !reflect.DeepEqual(req, p.received) {
		t.Fatal("trace altered provider request")
	}
	if len(captured) != 1 || captured[0].Kind != EventRequestContext || captured[0].TraceInvocationID != "owner" || captured[0].TraceStreamID == "" {
		t.Fatalf("trace-only request boundary=%+v", captured)
	}
	got := captured[0].RequestContext
	if got.System != req.System || len(got.Tools) != 1 || got.Tools[0].Name != "Read" || got.Model != "selected-model" || got.Provider != "fixture" {
		t.Fatalf("wrong request context: %+v", got)
	}
}
