package anthropic

// schema_placeholder_test.go pins the 2026-05-14 root-cause fix for
// MiniMax `/anthropic` gateway error code 2013 ("invalid function
// arguments json string"). When a tool's schema declares no required
// fields, the model is free to emit `arguments=null` or `""` on tool
// calls — MiniMax's strict reverse-serializer rejects either. metis
// injects a synthetic `_` string field marked as `required` so the
// model is forced to emit `{"_":""}`, which the gateway accepts. The
// tool's Execute() reads its real named fields by type-assertion and
// silently ignores `_`.
//
// Mirrors metis-cu's `noArgsSchema()` defense; metis previously only
// had a 2-attempt retry-with-reminder downstream (`withToolArgsReminder`)
// which hit ~80% recovery — this closes the remaining ~20%.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestNeedsEmptySchemaPlaceholder_MatchesMinimax(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://api.minimaxi.com/anthropic", true},
		{"https://api.minimaxi.com/anthropic/v1/messages", true},
		{"HTTPS://API.MINIMAXI.COM/anthropic", true},
		{"https://api.anthropic.com", false},
		{"https://api.deepseek.com/v1", false},
		{"", false},
		{"http://bedrock-runtime.us-east-1.amazonaws.com", false},
	}
	for _, c := range cases {
		if got := needsEmptySchemaPlaceholder(c.url); got != c.want {
			t.Errorf("needsEmptySchemaPlaceholder(%q) = %v; want %v", c.url, got, c.want)
		}
	}
}

func TestWithSchemaPlaceholder_EmptyRequired_InjectsField(t *testing.T) {
	// Mirrors MetisInfo / TaskList shape: one optional property, no required.
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"section": map[string]any{"type": "string"},
		},
	}
	out := withSchemaPlaceholder(schema)

	props, ok := out["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties not a map: %v", out["properties"])
	}
	if _, ok := props[emptySchemaPlaceholderField]; !ok {
		t.Errorf("expected `_` placeholder injected; properties = %v", props)
	}
	if _, ok := props["section"]; !ok {
		t.Errorf("original property `section` must be preserved; properties = %v", props)
	}
	req, _ := out["required"].([]any)
	if len(req) != 1 || req[0] != emptySchemaPlaceholderField {
		t.Errorf("expected required = [%q]; got %v", emptySchemaPlaceholderField, req)
	}
}

func TestWithSchemaPlaceholder_HasRequired_NoOp(t *testing.T) {
	// Mirrors Read / Edit shape: at least one required field.
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{"type": "string"},
		},
		"required": []any{"file_path"},
	}
	out := withSchemaPlaceholder(schema)

	props, _ := out["properties"].(map[string]any)
	if _, exists := props[emptySchemaPlaceholderField]; exists {
		t.Errorf("placeholder MUST NOT be injected when required is non-empty; properties = %v", props)
	}
	req, _ := out["required"].([]any)
	if len(req) != 1 || req[0] != "file_path" {
		t.Errorf("required must be preserved verbatim; got %v", req)
	}
}

func TestWithSchemaPlaceholder_StringRequired_AlsoCountsAsHasRequired(t *testing.T) {
	// Go-side constructed schemas can use []string instead of []any. Both
	// shapes must be recognized so we don't accidentally double-inject.
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{"type": "string"},
		},
		"required": []string{"file_path"},
	}
	out := withSchemaPlaceholder(schema)
	props, _ := out["properties"].(map[string]any)
	if _, exists := props[emptySchemaPlaceholderField]; exists {
		t.Errorf("[]string required should count; got injected placeholder: %v", props)
	}
}

func TestWithSchemaPlaceholder_NilSchema_BuildsFullObject(t *testing.T) {
	out := withSchemaPlaceholder(nil)
	if out["type"] != "object" {
		t.Errorf("nil input should produce type:object; got %v", out["type"])
	}
	props, _ := out["properties"].(map[string]any)
	if _, ok := props[emptySchemaPlaceholderField]; !ok {
		t.Errorf("nil input should still inject `_`; got %v", props)
	}
}

func TestWithSchemaPlaceholder_DoesNotMutateInput(t *testing.T) {
	props := map[string]any{
		"section": map[string]any{"type": "string"},
	}
	original := map[string]any{
		"type":       "object",
		"properties": props,
	}
	_ = withSchemaPlaceholder(original)
	// Original input map MUST be unchanged so the tool's shared
	// InputSchema() return value is safe across multiple calls.
	if _, exists := props[emptySchemaPlaceholderField]; exists {
		t.Errorf("input properties map was mutated; metis must clone before injecting")
	}
	if _, exists := original["required"]; exists {
		t.Errorf("input top-level map was mutated; original gained `required` key")
	}
}

func TestToAnthropicWithFlags_PlaceholderFlowsToToolSchema(t *testing.T) {
	// End-to-end through the conversion helper: when placeholderEmpty=true,
	// a tool with no required fields emerges with the `_` field on the
	// wire-format schema. When placeholderEmpty=false (default for real
	// Anthropic / Bedrock / Vertex), the schema passes through verbatim.
	req := Request{
		Tools: []ToolSpec{
			{
				Name:        "MetisInfo",
				Description: "introspect",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"section": map[string]any{"type": "string"},
					},
				},
			},
		},
	}

	// With placeholder injection on:
	on := toAnthropicWithFlags(req, "MiniMax-M2.7", 1024, false, false, true)
	if len(on.Tools) != 1 {
		t.Fatalf("expected 1 tool on output; got %d", len(on.Tools))
	}
	props, _ := on.Tools[0].InputSchema["properties"].(map[string]any)
	if _, ok := props[emptySchemaPlaceholderField]; !ok {
		t.Errorf("placeholderEmpty=true should inject `_` into MetisInfo schema; got %v", props)
	}

	// With placeholder injection off (real Anthropic, Bedrock, Vertex):
	off := toAnthropicWithFlags(req, "claude-sonnet-4-6", 1024, false, false, false)
	propsOff, _ := off.Tools[0].InputSchema["properties"].(map[string]any)
	if _, ok := propsOff[emptySchemaPlaceholderField]; ok {
		t.Errorf("placeholderEmpty=false MUST leave schema untouched; got injected `_`")
	}

	// Verify Provider import is wired correctly (compile-time guard).
	_ = provider.Request{}
}
