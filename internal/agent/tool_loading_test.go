package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestToolLoadingForkDiscoversOmittedTailAndDoesNotMutateParent(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "true")
	l := loadingLoop(144)
	p := l.Provider.(*limitedToolProvider)
	p.resps = []*llm.Response{
		{StopReason: "tool_use", Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "s", ToolName: "ToolSearch", ToolInput: map[string]any{"query": "select:remote_143"}}}},
		{StopReason: "tool_use", Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "t", ToolName: "remote_143", ToolInput: map[string]any{}}}},
		{StopReason: "end_turn", Content: []llm.ContentBlock{{Type: "text", Text: "done"}}},
	}
	result, err := RunForkedAgent(context.Background(), ForkedAgentParams{Cache: CacheSafeParams{Provider: p, ToolSpecs: l.toolSpecs()}, Prompt: "test", Registry: l.Registry, CanUseTool: AllowAll})
	if err != nil {
		t.Fatal(err)
	}
	if result.LastAssistantText() != "done" || len(p.calls) != 3 || containsName(p.calls[0].Tools, "remote_143") || !containsName(p.calls[1].Tools, "remote_143") {
		t.Fatal("fork continuation did not load missing schema")
	}
	for _, m := range result.NewMessages {
		for _, b := range m.Content {
			if b.IsError {
				t.Fatal(b.ToolResult)
			}
		}
	}
	if containsName(l.toolSpecs(), "remote_143") {
		t.Fatal("child discovery leaked into parent")
	}
}

func TestToolLoadingRestoreRetainsRecentSelectionWithinCap(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "true")
	l := loadingLoop(145)
	for i := 144; i >= 0; i-- {
		name := fmt.Sprintf("remote_%03d", i)
		body, _ := json.Marshal(map[string]any{"matches": []any{map[string]any{"name": name, "input_schema": map[string]any{"type": "object"}}}})
		id := fmt.Sprint(i)
		l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: id, ToolName: "ToolSearch"}}}, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: id, ToolResult: string(body)}}})
	}
	got := l.toolSpecs()
	if len(got) != 128 || !containsName(got, "remote_000") || containsName(got, "remote_144") {
		t.Fatal("history restore lost recency or exceeded cap")
	}
}

type limitedToolProvider struct{ scriptedProvider }

func (*limitedToolProvider) MaxToolDefinitions() int { return 128 }

func loadingLoop(n int) *Loop {
	r := tools.NewRegistry()
	r.Register(forkFakeTool{name: "Read"})
	for i := 0; i < n; i++ {
		r.Register(deferredForkTool{forkFakeTool{name: fmt.Sprintf("remote_%03d", i)}})
	}
	return &Loop{Registry: r, Provider: &limitedToolProvider{}}
}

func TestToolLoadingOmitsUnselectedButSearchesEntireCatalog(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "true")
	l := loadingLoop(144)
	if got := l.toolSpecs(); len(got) != 2 || !containsName(got, "Read") || !containsName(got, "ToolSearch") {
		t.Fatalf("unselected definitions leaked: %d", len(got))
	}
	result := handleToolSearch(l, llm.ContentBlock{ToolUseID: "search", ToolInput: map[string]any{"query": "select:remote_143"}})
	if result.IsError {
		t.Fatal(result.ToolResult)
	}
	if got := l.toolSpecs(); len(got) != 3 || !containsName(got, "remote_143") {
		t.Fatalf("selected tail tool unavailable: %+v", got)
	}
	if !l.deferredSchemaPending("remote_000") {
		t.Fatal("omitted tool bypasses discovery gate")
	}
}

func TestToolLoadingBoundsDiscoveryAndCanReloadEvictedTool(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "true")
	l := loadingLoop(145)
	for i := 0; i < 145; i++ {
		l.markDeferredDiscovered(fmt.Sprintf("remote_%03d", i))
	}
	got := l.toolSpecs()
	if len(got) != 128 || !containsName(got, "remote_144") || containsName(got, "remote_000") {
		t.Fatalf("unbounded or stale selection: %d", len(got))
	}
	l.markDeferredDiscovered("remote_000")
	got = l.toolSpecs()
	if len(got) != 128 || !containsName(got, "remote_000") || !containsName(got, "Read") || !containsName(got, "ToolSearch") {
		t.Fatal("evicted capability cannot be reloaded")
	}
	if len(l.Registry.ModelEntriesForCache()) != 146 {
		t.Fatal("projection modified full registry")
	}
}

func TestToolLoadingForcesDiscoveryWhenStandardExceedsEndpointLimit(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	l := loadingLoop(144)
	got := l.toolSpecs()
	if len(got) > 128 || !containsName(got, "ToolSearch") || !l.deferredSchemaPending("remote_000") {
		t.Fatalf("standard mode bypasses count admission: %d", len(got))
	}
}
