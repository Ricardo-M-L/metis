package agent

import (
	"reflect"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestWarmForkRestoreDropsRequestPin(t *testing.T) {
	for _, operation := range []string{"restore", "new-session", "reset", "provider"} {
		t.Run(operation, func(t *testing.T) {
			p := &queuedStreamProvider{}
			parent := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "parent", 12)
			parent.SystemSections = []llm.SystemSection{{Name: "base", Body: "parent base", Cache: true}}
			parent.CurrentStateSnapshot = func() RuntimeStateSnapshot {
				return RuntimeStateSnapshot{SessionID: "parent-affinity"}
			}
			parent.AppendUser("parent request")
			req, anchor := parent.buildRequestWithContext(nil)
			parent.rememberRequest(req, anchor)
			snap := parent.parentSnapshot("Fork")
			child := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 12)
			child.RestoreFork(snap, snap.Messages)
			child.System = "replacement"
			child.SystemSections = []llm.SystemSection{{Name: "base", Body: "replacement", Cache: true}}
			history := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "new request"}}}}
			wantAffinity := "parent-affinity"
			switch operation {
			case "new-session":
				child.ResetSession(history)
				wantAffinity = ""
			case "reset":
				child.Reset()
			case "provider":
				child.RebindProviderModel(&queuedStreamProvider{}, "replacement-model")
			default:
				child.Restore(history)
			}
			after := child.buildRequest(nil)
			if after.System != "replacement" || after.SessionID != wantAffinity || len(after.SystemSections) == 0 || after.SystemSections[0].Body != "replacement" {
				t.Fatalf("history/routing boundary retained a stale request or lost affinity: %+v", after)
			}
			if operation == "provider" && after.Model != "replacement-model" {
				t.Fatal("warm pin overrode a deliberate model switch")
			}
		})
	}
}

func TestWarmForkSnapshotDetachesSchemaAndHistory(t *testing.T) {
	p := &queuedStreamProvider{}
	parent := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "parent", 12)
	enum := []int{1, 2}
	toolInput := map[string]any{"values": []any{[]any{map[string]any{"text": "original"}}}}
	parent.Messages = []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "fixture", ToolName: "Fixture", ToolInput: toolInput}}}}
	specs := []llm.ToolSpec{{Name: "Fixture", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"enum": enum}}}}}
	req, anchor := parent.buildRequestWithContext(specs)
	parent.rememberRequest(req, anchor)
	snap := parent.parentSnapshot("Fork")
	enum[0] = 99
	toolInput["values"].([]any)[0].([]any)[0].(map[string]any)["text"] = "changed"
	child := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "", 12)
	child.Registry.Register(&fakeTool{name: "Fixture"})
	child.RestoreFork(snap, snap.Messages)
	actual := child.buildRequest(nil)
	got := actual.Tools[0].InputSchema["properties"].(map[string]any)["value"].(map[string]any)["enum"]
	if !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("parent schema mutation escaped its snapshot: %v", got)
	}
	gotText := actual.Messages[0].Content[0].ToolInput["values"].([]any)[0].([]any)[0].(map[string]any)["text"]
	if gotText != "original" {
		t.Fatalf("parent history mutation escaped its snapshot: %v", gotText)
	}
}

func TestRequestSnapshotClonesNamedNullableSchema(t *testing.T) {
	type choices []any
	values := choices{nil, map[string]any{"nested": choices{nil, "value"}}}
	req := llm.Request{Tools: []llm.ToolSpec{{Name: "Nullable", InputSchema: map[string]any{"enum": values}}}}
	cloned := cloneRequest(req)
	if !reflect.DeepEqual(cloned.Tools[0].InputSchema["enum"], values) {
		t.Fatal("named nullable choices were not preserved")
	}
	values[1].(map[string]any)["nested"].(choices)[1] = "changed"
	got := cloned.Tools[0].InputSchema["enum"].(choices)[1].(map[string]any)["nested"].(choices)[1]
	if got != "value" {
		t.Fatal("named nested choices still share mutable storage")
	}
}

func TestWarmForkSnapshotRejectsReboundProvider(t *testing.T) {
	p := &queuedStreamProvider{}
	parent := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "source", 12)
	parent.AppendUser("source")
	req, anchor := parent.buildRequestWithContext(nil)
	parent.rememberRequest(req, anchor)
	parent.RebindProviderModel(&queuedStreamProvider{}, "replacement")
	if snap := parent.parentSnapshot("Fork"); snap.request != nil {
		t.Fatal("a rebound provider inherited the previous route's request")
	}
}
