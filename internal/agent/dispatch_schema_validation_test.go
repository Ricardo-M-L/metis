package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

type schemaValidationProbeTool struct {
	canUseCalls atomic.Int64
	execCalls   atomic.Int64
}

type normalizingSchemaProbeTool struct{ *schemaValidationProbeTool }

func (*normalizingSchemaProbeTool) Name() string { return "NormalizingSchemaProbe" }
func (*normalizingSchemaProbeTool) NormalizeInput(input map[string]any) (map[string]any, error) {
	return pubtool.NormalizeAliases(input, map[string]string{"n": "count"})
}

type invalidSchemaProbeTool struct{ *schemaValidationProbeTool }

func (*invalidSchemaProbeTool) Name() string { return "InvalidSchemaProbe" }
func (*invalidSchemaProbeTool) InputSchema() map[string]any {
	return map[string]any{"type": "object", "required": "count"}
}

type secretRejectingSchemaProbeTool struct{ *schemaValidationProbeTool }

func (*secretRejectingSchemaProbeTool) Name() string { return "SecretRejectingSchemaProbe" }
func (*secretRejectingSchemaProbeTool) NormalizeInput(map[string]any) (map[string]any, error) {
	return nil, errors.New("normalizer inspected secret sk-live-dispatch-do-not-echo")
}

type rawSchemaProbeTool struct{ *schemaValidationProbeTool }

func (*rawSchemaProbeTool) Name() string { return "RawSchemaProbe" }
func (*rawSchemaProbeTool) InputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"required":             []string{"_raw"},
		"additionalProperties": false,
		"properties": map[string]any{
			"_raw": map[string]any{"type": "string"},
		},
	}
}

type rawSchemaProperties map[string]any

type namedRawPropertiesSchemaProbeTool struct{ *schemaValidationProbeTool }

func (*namedRawPropertiesSchemaProbeTool) Name() string { return "NamedRawPropertiesSchemaProbe" }
func (*namedRawPropertiesSchemaProbeTool) InputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"required":             []string{"_raw"},
		"additionalProperties": false,
		"properties": rawSchemaProperties{
			"_raw": map[string]any{"type": "string"},
		},
	}
}

type composedRawSchemaProbeTool struct {
	*schemaValidationProbeTool
	toolName string
	schema   map[string]any
}

func (t *composedRawSchemaProbeTool) Name() string                { return t.toolName }
func (t *composedRawSchemaProbeTool) InputSchema() map[string]any { return t.schema }

type rawProducingNormalizerProbeTool struct{ *schemaValidationProbeTool }

func (*rawProducingNormalizerProbeTool) Name() string { return "RawProducingNormalizerProbe" }
func (*rawProducingNormalizerProbeTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (*rawProducingNormalizerProbeTool) NormalizeInput(input map[string]any) (map[string]any, error) {
	if value, present := input["legacy_payload"]; present {
		return map[string]any{"_raw": value}, nil
	}
	return input, nil
}

func (*schemaValidationProbeTool) Name() string        { return "SchemaProbe" }
func (*schemaValidationProbeTool) Description() string { return "schema validation probe" }
func (*schemaValidationProbeTool) InputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"required":             []string{"count"},
		"additionalProperties": false,
		"properties": map[string]any{
			"count": map[string]any{"type": "integer"},
		},
	}
}
func (*schemaValidationProbeTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (p *schemaValidationProbeTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	p.canUseCalls.Add(1)
	return tools.PermissionAllow, ""
}
func (p *schemaValidationProbeTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	p.execCalls.Add(1)
	return &tools.Result{Output: "executed"}, nil
}
func (*schemaValidationProbeTool) IsEnabled() bool { return true }

func TestDispatchRejectsInvalidToolInputBeforePermissionOrExecute(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	reg := tools.NewRegistry()
	reg.Register(probe)
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}
	out := make(chan Event, 8)

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "bad-1", ToolName: probe.Name(),
		ToolInput: map[string]any{"count": "two"},
	}}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if probe.canUseCalls.Load() != 0 || probe.execCalls.Load() != 0 {
		t.Fatalf("invalid input crossed execution boundary: CanUse=%d Execute=%d", probe.canUseCalls.Load(), probe.execCalls.Load())
	}
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("results = %+v, want one error", results)
	}
	for _, want := range []string{"INVALID_TOOL_ARGS", "SchemaProbe", "$.count", "expected integer"} {
		if !strings.Contains(results[0].ToolResult, want) {
			t.Fatalf("tool error %q missing %q", results[0].ToolResult, want)
		}
	}
}

func TestDispatchRevalidatesPreToolUseModifiedInput(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	reg := tools.NewRegistry()
	reg.Register(probe)
	hooks := pubhook.NewRegistry()
	hooks.Register(pubhook.PreToolUseHandler(func(_ context.Context, _ pubhook.Context, _ *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
		return &pubhook.ModifiedPreToolUse{ModifiedInput: map[string]any{"count": "broken-by-hook"}}
	}))
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions), Hooks: hooks}

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "hook-1", ToolName: probe.Name(),
		ToolInput: map[string]any{"count": 1.0},
	}}, make(chan Event, 8), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if probe.canUseCalls.Load() != 0 || probe.execCalls.Load() != 0 {
		t.Fatalf("hook-invalid input crossed execution boundary: CanUse=%d Execute=%d", probe.canUseCalls.Load(), probe.execCalls.Load())
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, "$.count") {
		t.Fatalf("results = %+v, want precise validation error", results)
	}
}

func TestDispatchNormalizesAliasesBeforeValidationAndPermission(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &normalizingSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "alias-1", ToolName: tool.Name(),
		ToolInput: map[string]any{"n": 2},
	}}, make(chan Event, 8), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 || results[0].IsError || probe.canUseCalls.Load() != 1 || probe.execCalls.Load() != 1 {
		t.Fatalf("normalized dispatch failed: results=%+v CanUse=%d Execute=%d", results, probe.canUseCalls.Load(), probe.execCalls.Load())
	}
}

func TestDispatchNormalizesAliasesBeforePreToolUseHook(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &normalizingSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	hooks := pubhook.NewRegistry()
	var hookCalls atomic.Int64
	hooks.Register(pubhook.PreToolUseHandler(func(_ context.Context, _ pubhook.Context, call *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
		hookCalls.Add(1)
		if _, hasAlias := call.Input["n"]; hasAlias || call.Input["count"] != 2 {
			t.Errorf("PreToolUse saw non-canonical input: %#v", call.Input)
		}
		return nil
	}))
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions), Hooks: hooks}

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "hook-alias", ToolName: tool.Name(),
		ToolInput: map[string]any{"n": 2},
	}}, make(chan Event, 8), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 || results[0].IsError || hookCalls.Load() != 1 || probe.execCalls.Load() != 1 {
		t.Fatalf("canonical hook dispatch failed: results=%+v hooks=%d execute=%d", results, hookCalls.Load(), probe.execCalls.Load())
	}
}

func TestDispatchWritesFinalNormalizedHookInputBackToToolUses(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &normalizingSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	hooks := pubhook.NewRegistry()
	hooks.Register(pubhook.PreToolUseHandler(func(_ context.Context, _ pubhook.Context, _ *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
		return &pubhook.ModifiedPreToolUse{ModifiedInput: map[string]any{"n": 7}}
	}))
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions), Hooks: hooks}
	toolUses := []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "hook-writeback", ToolName: tool.Name(),
		ToolInput: map[string]any{"n": 2},
	}}

	results, err := loop.executeBatch(context.Background(), toolUses, make(chan Event, 8), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 || results[0].IsError || probe.execCalls.Load() != 1 {
		t.Fatalf("hook dispatch failed: results=%+v execute=%d", results, probe.execCalls.Load())
	}
	if _, hasAlias := toolUses[0].ToolInput["n"]; hasAlias || toolUses[0].ToolInput["count"] != 7 {
		t.Fatalf("caller toolUses did not receive final normalized hook input: %#v", toolUses[0].ToolInput)
	}
}

func TestDispatchWritesInitialNormalizedInputBackWhenHookShortCircuits(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &normalizingSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	hooks := pubhook.NewRegistry()
	hooks.Register(pubhook.PreToolUseHandler(func(_ context.Context, _ pubhook.Context, _ *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
		return &pubhook.ModifiedPreToolUse{Output: &pubhook.Output{Content: "intercepted"}}
	}))
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions), Hooks: hooks}
	toolUses := []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "hook-initial-writeback", ToolName: tool.Name(),
		ToolInput: map[string]any{"n": 2},
	}}

	results, err := loop.executeBatch(context.Background(), toolUses, make(chan Event, 8), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 || results[0].IsError || results[0].ToolResult != "intercepted" || probe.execCalls.Load() != 0 {
		t.Fatalf("hook short-circuit failed: results=%+v execute=%d", results, probe.execCalls.Load())
	}
	if _, hasAlias := toolUses[0].ToolInput["n"]; hasAlias || toolUses[0].ToolInput["count"] != 2 {
		t.Fatalf("caller toolUses did not receive initial normalized input: %#v", toolUses[0].ToolInput)
	}
}

func TestDispatchNormalizationErrorDoesNotEchoCustomError(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &secretRejectingSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}
	const secret = "sk-live-dispatch-do-not-echo"

	out := make(chan Event, 8)
	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "secret-normalization", ToolName: tool.Name(),
		ToolInput: map[string]any{"count": 1, "token": "ordinary input"},
	}}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("results = %+v, want normalization error", results)
	}
	if strings.Contains(results[0].ToolResult, secret) {
		t.Fatalf("normalizer error leaked into model result: %q", results[0].ToolResult)
	}
	if !strings.Contains(results[0].ToolResult, "normalization") {
		t.Fatalf("result lacks structured normalization guidance: %q", results[0].ToolResult)
	}
	if results[0].Presentation["keyword"] != "normalization" || results[0].Presentation["action"] != "rebuild_arguments" {
		t.Fatalf("normalization presentation is not structured safely: %#v", results[0].Presentation)
	}
	for len(out) > 0 {
		event := <-out
		if event.ToolResult != nil && strings.Contains(event.ToolResult.Output, secret) {
			t.Fatalf("normalizer error leaked into event output: %q", event.ToolResult.Output)
		}
	}
	if probe.canUseCalls.Load() != 0 || probe.execCalls.Load() != 0 {
		t.Fatalf("normalization error crossed execution boundary: CanUse=%d Execute=%d", probe.canUseCalls.Load(), probe.execCalls.Load())
	}
}

func TestDispatchInvalidToolSchemaFailsClosedWithoutRetryAdvice(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &invalidSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "schema-bad", ToolName: tool.Name(),
		ToolInput: map[string]any{"count": 2},
	}}, make(chan Event, 8), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if probe.canUseCalls.Load() != 0 || probe.execCalls.Load() != 0 {
		t.Fatalf("invalid schema crossed execution boundary: CanUse=%d Execute=%d", probe.canUseCalls.Load(), probe.execCalls.Load())
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, pubtool.ValidationCodeSchemaInvalid) {
		t.Fatalf("result = %+v, want TOOL_SCHEMA_INVALID", results)
	}
	if retryable, _ := results[0].Presentation["retryable"].(bool); retryable {
		t.Fatalf("schema defect incorrectly marked retryable: %+v", results[0].Presentation)
	}
}

func TestDispatchRejectsMalformedToolJSONWithoutEchoingRawInput(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	reg := tools.NewRegistry()
	reg.Register(probe)
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}
	const secret = "do-not-echo"

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "raw-1", ToolName: probe.Name(),
		ToolInput: map[string]any{}, ToolInputMalformed: true,
	}}, make(chan Event, 8), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if probe.canUseCalls.Load() != 0 || probe.execCalls.Load() != 0 {
		t.Fatalf("malformed JSON crossed execution boundary: CanUse=%d Execute=%d", probe.canUseCalls.Load(), probe.execCalls.Load())
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, "INVALID_JSON") {
		t.Fatalf("results = %+v, want INVALID_JSON", results)
	}
	if strings.Contains(results[0].ToolResult, secret) {
		t.Fatalf("malformed raw input leaked into tool result: %q", results[0].ToolResult)
	}
}

func TestDispatchRejectsLegacySoleRawWhenSchemaDoesNotDeclareIt(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	reg := tools.NewRegistry()
	reg.Register(probe)
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}
	const secret = "legacy-raw-secret-do-not-echo"

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "legacy-raw", ToolName: probe.Name(),
		ToolInput: map[string]any{"_raw": secret},
	}}, make(chan Event, 8), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, invalidToolJSONCode) {
		t.Fatalf("legacy raw input = %+v, want INVALID_JSON", results)
	}
	if strings.Contains(results[0].ToolResult, secret) {
		t.Fatalf("legacy raw input leaked into tool result: %q", results[0].ToolResult)
	}
	if probe.canUseCalls.Load() != 0 || probe.execCalls.Load() != 0 {
		t.Fatalf("legacy raw input crossed execution boundary: CanUse=%d Execute=%d", probe.canUseCalls.Load(), probe.execCalls.Load())
	}
}

func TestDispatchAllowsSoleRawWhenToolSchemaDeclaresIt(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &rawSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "raw-schema", ToolName: tool.Name(),
		ToolInput: map[string]any{"_raw": "legitimate payload"},
	}}, make(chan Event, 8), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 || results[0].IsError || probe.canUseCalls.Load() != 1 || probe.execCalls.Load() != 1 {
		t.Fatalf("declared _raw input did not execute: results=%+v CanUse=%d Execute=%d", results, probe.canUseCalls.Load(), probe.execCalls.Load())
	}
}

func TestDispatchAllowsSoleRawWhenNamedPropertiesMapDeclaresIt(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &namedRawPropertiesSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "named-raw-schema", ToolName: tool.Name(),
		ToolInput: map[string]any{"_raw": "legitimate payload"},
	}}, make(chan Event, 8), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 || results[0].IsError || probe.canUseCalls.Load() != 1 || probe.execCalls.Load() != 1 {
		t.Fatalf("named properties _raw input did not execute: results=%+v CanUse=%d Execute=%d", results, probe.canUseCalls.Load(), probe.execCalls.Load())
	}
}

func TestDispatchAllowsSoleRawDeclaredThroughLocalRefOrAllOf(t *testing.T) {
	directDeclaration := map[string]any{
		"type":                 "object",
		"required":             []string{"_raw"},
		"additionalProperties": false,
		"properties": map[string]any{
			"_raw": map[string]any{"type": "string"},
		},
	}
	tests := []struct {
		name   string
		schema map[string]any
	}{
		{
			name: "local ref",
			schema: map[string]any{
				"$defs": map[string]any{"payload": directDeclaration},
				"$ref":  "#/$defs/payload",
			},
		},
		{
			name:   "allOf",
			schema: map[string]any{"allOf": []any{directDeclaration}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probe := &schemaValidationProbeTool{}
			tool := &composedRawSchemaProbeTool{
				schemaValidationProbeTool: probe,
				toolName:                  "ComposedRawSchemaProbe",
				schema:                    tc.schema,
			}
			reg := tools.NewRegistry()
			reg.Register(tool)
			loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}

			results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
				Type: "tool_use", ToolUseID: "composed-raw-schema", ToolName: tool.Name(),
				ToolInput: map[string]any{"_raw": "legitimate payload"},
			}}, make(chan Event, 8), HookContext{})
			if err != nil {
				t.Fatalf("executeBatch: %v", err)
			}
			if len(results) != 1 || results[0].IsError || probe.canUseCalls.Load() != 1 || probe.execCalls.Load() != 1 {
				t.Fatalf("composed _raw input did not execute: results=%+v CanUse=%d Execute=%d", results, probe.canUseCalls.Load(), probe.execCalls.Load())
			}
		})
	}
}

func TestSchemaDeclaresTopLevelPropertyDoesNotTrustNestedOrPermissiveAlternative(t *testing.T) {
	nested := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"container": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"_raw": map[string]any{"type": "string"},
				},
			},
		},
	}
	if schemaDeclaresTopLevelProperty(nested, "_raw") {
		t.Fatal("nested _raw declaration was mistaken for a root property")
	}

	permissiveAlternative := map[string]any{
		"anyOf": []any{
			map[string]any{"properties": map[string]any{"_raw": map[string]any{"type": "integer"}}},
			map[string]any{},
		},
	}
	if schemaDeclaresTopLevelProperty(permissiveAlternative, "_raw") {
		t.Fatal("one declaring anyOf branch allowed a permissive sibling to trust legacy _raw")
	}
}

func TestDispatchRejectsLegacySoleRawProducedByNormalizer(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &rawProducingNormalizerProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	hooks := pubhook.NewRegistry()
	var hookCalls atomic.Int64
	hooks.Register(pubhook.PreToolUseHandler(func(context.Context, pubhook.Context, *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
		hookCalls.Add(1)
		return &pubhook.ModifiedPreToolUse{Output: &pubhook.Output{Content: "hook must not bypass malformed input"}}
	}))
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions), Hooks: hooks}
	const secret = "normalized-legacy-raw-secret"

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "normalized-legacy-raw", ToolName: tool.Name(),
		ToolInput: map[string]any{"legacy_payload": secret},
	}}, make(chan Event, 8), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, invalidToolJSONCode) {
		t.Fatalf("normalized legacy raw = %+v, want INVALID_JSON", results)
	}
	if strings.Contains(results[0].ToolResult, secret) {
		t.Fatalf("normalized legacy raw leaked into tool result: %q", results[0].ToolResult)
	}
	if hookCalls.Load() != 0 || probe.canUseCalls.Load() != 0 || probe.execCalls.Load() != 0 {
		t.Fatalf("normalized legacy raw crossed boundary: Hook=%d CanUse=%d Execute=%d", hookCalls.Load(), probe.canUseCalls.Load(), probe.execCalls.Load())
	}
}

func TestDispatchExplicitMalformedMarkerOverridesDeclaredRawSchema(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &rawSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "raw-marked", ToolName: tool.Name(),
		ToolInput: map[string]any{"_raw": "legitimate-looking payload"}, ToolInputMalformed: true,
	}}, make(chan Event, 8), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, invalidToolJSONCode) {
		t.Fatalf("marked input = %+v, want INVALID_JSON", results)
	}
	if probe.canUseCalls.Load() != 0 || probe.execCalls.Load() != 0 {
		t.Fatalf("marked input crossed execution boundary: CanUse=%d Execute=%d", probe.canUseCalls.Load(), probe.execCalls.Load())
	}
}

func TestLoopReturnsSchemaErrorThenExecutesCorrectedRetry(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	reg := tools.NewRegistry()
	reg.Register(probe)
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		toolUseStream("bad-count", probe.Name(), `{"count":"two"}`),
		toolUseStream("good-count", probe.Name(), `{"count":2}`),
		textStream("corrected and complete"),
	}}
	loop := NewLoop(provider, reg, permission.New(permission.ModeBypassPermissions), NewHookRegistry(), "system", 6)
	loop.AppendUser("run the schema probe")
	if err := loop.Run(context.Background(), make(chan Event, 64)); err != nil {
		t.Fatalf("Loop.Run: %v", err)
	}
	if probe.canUseCalls.Load() != 1 || probe.execCalls.Load() != 1 {
		t.Fatalf("corrected retry counts: CanUse=%d Execute=%d, want 1/1", probe.canUseCalls.Load(), probe.execCalls.Load())
	}
	requests := provider.capturedRequests()
	if len(requests) != 3 {
		t.Fatalf("provider requests = %d, want invalid call + corrected call + final", len(requests))
	}
	if !requestContains(requests[1], "INVALID_TOOL_ARGS") || !requestContains(requests[1], "$.count") {
		t.Fatalf("second request did not receive precise correction feedback: %+v", requests[1].Messages)
	}
	// The assistant tool call itself remains in protocol history, but the
	// validation result must never echo the rejected value.
	for _, message := range requests[1].Messages {
		for _, block := range message.Content {
			if block.Type == "tool_result" && strings.Contains(block.ToolResult, `"count":"two"`) {
				t.Fatalf("validation result echoed rejected input: %q", block.ToolResult)
			}
		}
	}
}
