package openai

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestCacheAffinitySurvivesDeferredSchemaAndRuntimeChanges(t *testing.T) {
	r := NewResponses("", "https://api.openai.com/v1", "test", 64, 0, 0)
	r.StateMode = ResponsesStateLocal
	req := provider.Request{SessionID: "session-fixture", SystemSections: []provider.SystemSection{{Name: "base", Body: "stable"}, {Name: "runtime", Body: "one", Volatile: true}}}
	first, err := r.buildResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	req.Tools = []provider.ToolSpec{{Name: "NewDeferred", Description: "new schema"}}
	req.SystemSections[1].Body = "two"
	next, err := r.buildResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if first.PromptCacheKey != "session-fixture" || first.PromptCacheKey != next.PromptCacheKey {
		t.Fatal("session affinity changed with schema/runtime")
	}
	r.Capabilities.PromptCaching = false
	next, _ = r.buildResponsesRequest(req)
	if next.PromptCacheKey != "" {
		t.Fatal("unsupported cache key emitted")
	}
	if stablePromptCacheKey("test", "stable", nil) != stablePromptCacheKey("test", "stable", req.Tools) {
		t.Fatal("fallback affinity uses entire deferred tool inventory")
	}
}

func TestDynamicCheckpointReplayAndProviderReset(t *testing.T) {
	checkpoint := func(text string) provider.Message {
		return provider.Message{Role: provider.RoleSystem, Content: []provider.ContentBlock{{Type: "text", Text: text, ProviderHint: map[string]string{"metis.dynamic_context": "checkpoint"}}}}
	}
	r := NewResponses("", "https://api.openai.com/v1", "test", 64, 0, 0)
	r.StateMode = ResponsesStateLocal
	req := provider.Request{System: "stable", Messages: []provider.Message{checkpoint("old-state"), {Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "hello"}}}, checkpoint("new-state")}}
	body, err := r.buildResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 3 || body.Input[0].Role != "developer" || body.Input[2].Role != "developer" || body.Instructions != "stable" {
		t.Fatal("checkpoint replay lost position/authority")
	}
	r.StateMode = ResponsesStateProvider
	body, err = r.buildResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.Instructions, "old-state") || !strings.Contains(body.Instructions, "new-state") || len(body.Input) != 1 {
		t.Fatal("provider reset accumulated old snapshots")
	}
	if r.DynamicContextCheckpoints() {
		t.Fatal("managed provider should replace instructions")
	}
	chat := toOpenAI(req, "test", 64)
	if len(chat.Messages) != 4 || chat.Messages[1].Role != "system" || chat.Messages[3].Role != "system" {
		t.Fatal("chat downgraded/dropped checkpoint authority")
	}
}
