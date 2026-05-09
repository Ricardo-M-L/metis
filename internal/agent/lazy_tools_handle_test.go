package agent

// lazy_tools_handle_test.go covers handleToolSearch — the inline
// resolver dispatch.go calls when the model invokes the synthetic
// `ToolSearch` meta-tool. Existing lazy_tools_test.go covers the
// schema-stripping side (applyLazySchema); this file covers the
// fetch-on-demand side.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// fakeMCPTool is a minimal Tool impl whose schema we expect the
// ToolSearch path to round-trip back to the model. Embedded directly
// here rather than reused from another test file to keep this test
// self-contained; the package has several tiny stub tools and they
// drift independently.
type fakeMCPTool struct {
	name        string
	description string
	schema      map[string]any
}

func (f fakeMCPTool) Name() string                                 { return f.name }
func (f fakeMCPTool) Description() string                          { return f.description }
func (f fakeMCPTool) InputSchema() map[string]any                  { return f.schema }
func (f fakeMCPTool) Concurrency(map[string]any) pubtool.Concurrency { return pubtool.ConcurrencySafe }
func (f fakeMCPTool) CanUse(context.Context, map[string]any) (pubtool.Permission, string) {
	return pubtool.PermissionAllow, "test"
}
func (f fakeMCPTool) Execute(context.Context, map[string]any) (*pubtool.Result, error) {
	return &pubtool.Result{Output: "ok"}, nil
}

func newLoopWithTool(t *testing.T, name string, schema map[string]any) *Loop {
	t.Helper()
	reg := tools.NewRegistry()
	reg.Register(fakeMCPTool{
		name:        name,
		description: "fake mcp tool for testing",
		schema:      schema,
	})
	return &Loop{Registry: reg}
}

// TestHandleToolSearch_ReturnsSchemaForKnownTool — the happy path:
// model asks for `mcp__foo__bar`, we return its full schema.
func TestHandleToolSearch_ReturnsSchemaForKnownTool(t *testing.T) {
	wantSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
		"required": []string{"path"},
	}
	l := newLoopWithTool(t, "mcp__foo__bar", wantSchema)

	in := llm.ContentBlock{
		Type:      "tool_use",
		ToolUseID: "tu-1",
		ToolName:  "ToolSearch",
		ToolInput: map[string]any{"name": "mcp__foo__bar"},
	}
	got := handleToolSearch(l, in)

	if got.Type != "tool_result" || got.ToolUseID != "tu-1" {
		t.Errorf("wrong wrapper: type=%q id=%q", got.Type, got.ToolUseID)
	}
	if got.IsError {
		t.Fatalf("unexpected error result: %s", got.ToolResult)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got.ToolResult), &parsed); err != nil {
		t.Fatalf("result not valid JSON: %v\nbody: %s", err, got.ToolResult)
	}
	if parsed["name"] != "mcp__foo__bar" {
		t.Errorf("name field wrong: %v", parsed["name"])
	}
	if _, ok := parsed["input_schema"].(map[string]any); !ok {
		t.Errorf("input_schema should be an object; got %T", parsed["input_schema"])
	}
}

// TestHandleToolSearch_ErrorOnMissingName — the model is supposed to
// pass {"name": "..."}; if it forgets the field, return a clear
// error rather than crashing or returning empty.
func TestHandleToolSearch_ErrorOnMissingName(t *testing.T) {
	l := newLoopWithTool(t, "mcp__foo", map[string]any{"type": "object"})
	in := llm.ContentBlock{
		Type:      "tool_use",
		ToolUseID: "tu-2",
		ToolName:  "ToolSearch",
		ToolInput: map[string]any{},
	}
	got := handleToolSearch(l, in)
	if !got.IsError {
		t.Errorf("expected IsError=true when name field missing; got %+v", got)
	}
	if !strings.Contains(got.ToolResult, "name") {
		t.Errorf("error should mention the missing field; got %q", got.ToolResult)
	}
}

// TestHandleToolSearch_UnknownToolReturnsError — the model picked a
// name that's not in the registry. Distinct error shape from the
// missing-field case so log-readers can tell them apart.
func TestHandleToolSearch_UnknownToolReturnsError(t *testing.T) {
	l := newLoopWithTool(t, "mcp__real", map[string]any{"type": "object"})
	in := llm.ContentBlock{
		Type:      "tool_use",
		ToolUseID: "tu-3",
		ToolName:  "ToolSearch",
		ToolInput: map[string]any{"name": "mcp__nonexistent"},
	}
	got := handleToolSearch(l, in)
	if !got.IsError {
		t.Fatalf("expected IsError=true for unknown tool; got %+v", got)
	}
	if !strings.Contains(got.ToolResult, "mcp__nonexistent") {
		t.Errorf("error should echo the looked-up name; got %q", got.ToolResult)
	}
}

// TestHandleToolSearch_PreservesToolUseID — the result must carry the
// caller's tool_use_id verbatim so the LLM can pair it back to its
// pending tool_use block. Lose this and the conversation breaks
// invariant ("tool_result without matching tool_use").
func TestHandleToolSearch_PreservesToolUseID(t *testing.T) {
	l := newLoopWithTool(t, "mcp__foo", map[string]any{"type": "object"})
	for _, id := range []string{"toolu_01abc", "tu_xyz_999", "weird-id-with-dashes"} {
		t.Run(id, func(t *testing.T) {
			in := llm.ContentBlock{
				Type:      "tool_use",
				ToolUseID: id,
				ToolName:  "ToolSearch",
				ToolInput: map[string]any{"name": "mcp__foo"},
			}
			got := handleToolSearch(l, in)
			if got.ToolUseID != id {
				t.Errorf("ToolUseID mangled: %q -> %q", id, got.ToolUseID)
			}
		})
	}
}

// TestApplyLazySchema_AppendsToolSearchEntry — pin that the synthetic
// meta-tool is appended (not prepended, not inserted middle) so the
// cache breakpoint between built-ins and MCP tools stays placed
// correctly. cf. dispatch.go::toolSpecs comment.
func TestApplyLazySchema_AppendsToolSearchEntry(t *testing.T) {
	specs := []llm.ToolSpec{
		{Name: "Read"},
		{Name: "Bash"},
	}
	for i := 0; i < 25; i++ {
		specs = append(specs, llm.ToolSpec{
			Name:        "mcp__server__tool_" + string(rune('a'+i)),
			InputSchema: map[string]any{"type": "object"},
		})
	}
	// stripAndAppendToolSearch is the rewriter the trigger functions
	// dispatch to. Test it directly rather than picking a particular
	// trigger.
	out := stripAndAppendToolSearch(specs)

	if last := out[len(out)-1]; last.Name != "ToolSearch" {
		t.Fatalf("ToolSearch should be last; got last name %q (full slice %d entries)", last.Name, len(out))
	}
	// First two entries must remain Read / Bash unchanged (built-ins
	// keep full schemas, only mcp__* get stripped).
	if out[0].Name != "Read" || out[1].Name != "Bash" {
		t.Errorf("built-ins shifted: %q %q", out[0].Name, out[1].Name)
	}
}
