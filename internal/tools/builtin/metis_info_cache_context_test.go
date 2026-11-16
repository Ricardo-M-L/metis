package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestMetisInfoCacheSeparatesFingerprintChangeFromMiss(t *testing.T) {
	p := &cacheInfoProvider{}
	registry := tools.NewRegistry()
	registry.Register(NewMetisInfo(nil, nil, nil, nil, registry))
	l := agent.NewLoop(p, registry, permission.New(permission.ModeBypass), nil, "fixture-private-system-do-not-print", 8)
	l.CacheStats.Add(agent.CacheStat{Input: 100, CacheRead: 400, Fingerprint: "prior-request"})
	l.AppendUser("inspect cache")
	if err := l.Run(context.Background(), make(chan agent.Event, 128)); err != nil {
		t.Fatal(err)
	}
	var report string
	for _, message := range l.History() {
		for _, block := range message.Content {
			if block.Type == "tool_result" && block.ToolUseID == "cache-info" {
				report = block.ToolResult
			}
		}
	}
	for _, want := range []string{"[cache]", "fingerprint_changed = true", "actual_cache_read = 400", "cache_miss = unknown"} {
		if !strings.Contains(report, want) {
			t.Errorf("cache introspection lacks %q: %s", want, report)
		}
	}
	if strings.Contains(report, "fixture-private-system-do-not-print") {
		t.Fatal("cache diagnostics exposed prompt contents")
	}
}

func TestMetisInfoCacheWithoutLoopReportsUnavailable(t *testing.T) {
	tool := NewMetisInfo(nil, nil, nil, nil, nil)
	result, err := tool.Execute(context.Background(), map[string]any{"section": "cache"})
	if err != nil || !strings.Contains(result.Output, "available = false") {
		t.Fatalf("missing cache telemetry was not explicit: result=%+v err=%v", result, err)
	}
}

type cacheInfoProvider struct {
	fakeProvider
	calls int
}

func (p *cacheInfoProvider) Stream(_ context.Context, _ llm.Request) (llm.StreamReader, error) {
	p.calls++
	if p.calls == 1 {
		return &fakeStream{events: []llm.StreamEvent{
			{Type: "message_start", InputTokens: 100, CacheReadInputTokens: 400},
			{Type: "tool_use_start", ToolUseID: "cache-info", ToolName: "MetisInfo"},
			{Type: "tool_input_delta", ToolUseID: "cache-info", InputDelta: `{"section":"cache"}`},
			{Type: "tool_use_stop", ToolUseID: "cache-info"},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}}, nil
	}
	return helloProvider().Stream(context.Background(), llm.Request{})
}

func TestMetisInfoCacheUsesInvokingContextBeforeRegistryBinding(t *testing.T) {
	tool := NewMetisInfo(nil, nil, nil, nil, nil).WithCacheStats(fixtureCacheSummary{})
	ctx := tools.WithCacheSummary(context.Background(), "[cache]\nactual_cache_read = 123\n")
	result, err := tool.Execute(ctx, map[string]any{"section": "cache"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, "actual_cache_read = 123") || strings.Contains(result.Output, "900") {
		t.Fatalf("cache report used another loop's registry binding: %s", result.Output)
	}
	empty := tools.WithCacheSummary(context.Background(), "")
	result, err = tool.Execute(empty, map[string]any{"section": "cache"})
	if err != nil || !strings.Contains(result.Output, "available = false") || strings.Contains(result.Output, "900") {
		t.Fatalf("empty invocation inherited another loop's statistics: result=%+v err=%v", result, err)
	}
}
