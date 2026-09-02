package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestExecuteForkToolNormalizesBeforeGateAndExecute(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &normalizingSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	var gateCalls atomic.Int64
	result := executeForkTool(context.Background(), ForkedAgentParams{
		Registry: reg,
		CanUseTool: func(_ context.Context, _ string, input map[string]any) (bool, string) {
			gateCalls.Add(1)
			if _, hasAlias := input["n"]; hasAlias || input["count"] != 2 {
				t.Errorf("fork gate saw non-canonical input: %#v", input)
			}
			return true, ""
		},
	}, llm.ContentBlock{
		Type: "tool_use", ToolUseID: "fork-alias", ToolName: tool.Name(),
		ToolInput: map[string]any{"n": 2},
	}, nil)
	if result.IsError || gateCalls.Load() != 1 || probe.execCalls.Load() != 1 {
		t.Fatalf("fork normalized result=%+v gate=%d execute=%d", result, gateCalls.Load(), probe.execCalls.Load())
	}
}

func TestExecuteForkToolRejectsNormalizationConflictBeforeGate(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &normalizingSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	var gateCalls atomic.Int64
	result := executeForkTool(context.Background(), ForkedAgentParams{
		Registry: reg,
		CanUseTool: func(context.Context, string, map[string]any) (bool, string) {
			gateCalls.Add(1)
			return true, ""
		},
	}, llm.ContentBlock{
		Type: "tool_use", ToolUseID: "fork-conflict", ToolName: tool.Name(),
		ToolInput: map[string]any{"count": 1, "n": 2},
	}, nil)
	if !result.IsError || !strings.Contains(result.ToolResult, "normalization") {
		t.Fatalf("conflict result = %+v", result)
	}
	if gateCalls.Load() != 0 || probe.execCalls.Load() != 0 {
		t.Fatalf("conflict crossed fork boundary: gate=%d execute=%d", gateCalls.Load(), probe.execCalls.Load())
	}
}

func TestExecuteForkToolNormalizationErrorDoesNotEchoCustomError(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &secretRejectingSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	const secret = "sk-live-dispatch-do-not-echo"

	result := executeForkTool(context.Background(), ForkedAgentParams{
		Registry: reg, CanUseTool: AllowAll,
	}, llm.ContentBlock{
		Type: "tool_use", ToolUseID: "fork-secret-normalization", ToolName: tool.Name(),
		ToolInput: map[string]any{"count": 1, "token": "ordinary input"},
	}, nil)
	if !result.IsError || !strings.Contains(result.ToolResult, "normalization") {
		t.Fatalf("result = %+v, want structured normalization error", result)
	}
	if strings.Contains(result.ToolResult, secret) {
		t.Fatalf("normalizer error leaked into fork model result: %q", result.ToolResult)
	}
	if probe.execCalls.Load() != 0 {
		t.Fatalf("normalization error reached fork Execute: %d", probe.execCalls.Load())
	}
}

func TestExecuteForkToolAllowsSoleRawWhenToolSchemaDeclaresIt(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &rawSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	var gateCalls atomic.Int64

	result := executeForkTool(context.Background(), ForkedAgentParams{
		Registry: reg,
		CanUseTool: func(context.Context, string, map[string]any) (bool, string) {
			gateCalls.Add(1)
			return true, ""
		},
	}, llm.ContentBlock{
		Type: "tool_use", ToolUseID: "fork-raw-schema", ToolName: tool.Name(),
		ToolInput: map[string]any{"_raw": "legitimate payload"},
	}, nil)
	if result.IsError || gateCalls.Load() != 1 || probe.execCalls.Load() != 1 {
		t.Fatalf("declared fork _raw input did not execute: result=%+v gate=%d Execute=%d", result, gateCalls.Load(), probe.execCalls.Load())
	}
}

func TestExecuteForkToolAllowsSoleRawDeclaredThroughLocalRef(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &composedRawSchemaProbeTool{
		schemaValidationProbeTool: probe,
		toolName:                  "ForkLocalRefRawSchemaProbe",
		schema: map[string]any{
			"$defs": map[string]any{
				"payload": map[string]any{
					"type":                 "object",
					"required":             []string{"_raw"},
					"additionalProperties": false,
					"properties": map[string]any{
						"_raw": map[string]any{"type": "string"},
					},
				},
			},
			"$ref": "#/$defs/payload",
		},
	}
	reg := tools.NewRegistry()
	reg.Register(tool)
	var gateCalls atomic.Int64

	result := executeForkTool(context.Background(), ForkedAgentParams{
		Registry: reg,
		CanUseTool: func(context.Context, string, map[string]any) (bool, string) {
			gateCalls.Add(1)
			return true, ""
		},
	}, llm.ContentBlock{
		Type: "tool_use", ToolUseID: "fork-local-ref-raw", ToolName: tool.Name(),
		ToolInput: map[string]any{"_raw": "legitimate payload"},
	}, nil)
	if result.IsError || gateCalls.Load() != 1 || probe.execCalls.Load() != 1 {
		t.Fatalf("local-ref fork _raw input did not execute: result=%+v gate=%d Execute=%d", result, gateCalls.Load(), probe.execCalls.Load())
	}
}

func TestExecuteForkToolRejectsLegacySoleRawWhenSchemaDoesNotDeclareIt(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	reg := tools.NewRegistry()
	reg.Register(probe)
	var gateCalls atomic.Int64
	const secret = "fork-legacy-raw-secret-do-not-echo"

	result := executeForkTool(context.Background(), ForkedAgentParams{
		Registry: reg,
		CanUseTool: func(context.Context, string, map[string]any) (bool, string) {
			gateCalls.Add(1)
			return true, ""
		},
	}, llm.ContentBlock{
		Type: "tool_use", ToolUseID: "fork-legacy-raw", ToolName: probe.Name(),
		ToolInput: map[string]any{"_raw": secret},
	}, nil)
	if !result.IsError || !strings.Contains(result.ToolResult, invalidToolJSONCode) {
		t.Fatalf("result = %+v, want INVALID_JSON", result)
	}
	if strings.Contains(result.ToolResult, secret) {
		t.Fatalf("legacy raw input leaked into fork result: %q", result.ToolResult)
	}
	if gateCalls.Load() != 0 || probe.execCalls.Load() != 0 {
		t.Fatalf("legacy raw input crossed fork boundary: gate=%d Execute=%d", gateCalls.Load(), probe.execCalls.Load())
	}
}

func TestExecuteForkToolExplicitMalformedMarkerOverridesDeclaredRawSchema(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &rawSchemaProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	var gateCalls atomic.Int64

	result := executeForkTool(context.Background(), ForkedAgentParams{
		Registry: reg,
		CanUseTool: func(context.Context, string, map[string]any) (bool, string) {
			gateCalls.Add(1)
			return true, ""
		},
	}, llm.ContentBlock{
		Type: "tool_use", ToolUseID: "fork-raw-marked", ToolName: tool.Name(),
		ToolInput: map[string]any{"_raw": "legitimate-looking payload"}, ToolInputMalformed: true,
	}, nil)
	if !result.IsError || !strings.Contains(result.ToolResult, invalidToolJSONCode) {
		t.Fatalf("result = %+v, want INVALID_JSON", result)
	}
	if gateCalls.Load() != 0 || probe.execCalls.Load() != 0 {
		t.Fatalf("marked input crossed fork boundary: gate=%d Execute=%d", gateCalls.Load(), probe.execCalls.Load())
	}
}

func TestExecuteForkToolRejectsLegacySoleRawProducedByNormalizer(t *testing.T) {
	probe := &schemaValidationProbeTool{}
	tool := &rawProducingNormalizerProbeTool{schemaValidationProbeTool: probe}
	reg := tools.NewRegistry()
	reg.Register(tool)
	var gateCalls atomic.Int64
	const secret = "fork-normalized-legacy-raw-secret"

	result := executeForkTool(context.Background(), ForkedAgentParams{
		Registry: reg,
		CanUseTool: func(context.Context, string, map[string]any) (bool, string) {
			gateCalls.Add(1)
			return true, ""
		},
	}, llm.ContentBlock{
		Type: "tool_use", ToolUseID: "fork-normalized-legacy-raw", ToolName: tool.Name(),
		ToolInput: map[string]any{"legacy_payload": secret},
	}, nil)
	if !result.IsError || !strings.Contains(result.ToolResult, invalidToolJSONCode) {
		t.Fatalf("result = %+v, want INVALID_JSON", result)
	}
	if strings.Contains(result.ToolResult, secret) {
		t.Fatalf("normalized legacy raw leaked into fork result: %q", result.ToolResult)
	}
	if gateCalls.Load() != 0 || probe.execCalls.Load() != 0 {
		t.Fatalf("normalized legacy raw crossed fork boundary: gate=%d Execute=%d", gateCalls.Load(), probe.execCalls.Load())
	}
}

type forkPanicTool struct{ *schemaValidationProbeTool }

func (*forkPanicTool) Name() string { return "ForkPanic" }
func (*forkPanicTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	panic("boom")
}

func TestExecuteForkToolRecoversToolPanic(t *testing.T) {
	tool := &forkPanicTool{schemaValidationProbeTool: &schemaValidationProbeTool{}}
	reg := tools.NewRegistry()
	reg.Register(tool)
	result := executeForkTool(context.Background(), ForkedAgentParams{
		Registry: reg, CanUseTool: AllowAll,
	}, llm.ContentBlock{
		Type: "tool_use", ToolUseID: "fork-panic", ToolName: tool.Name(),
		ToolInput: map[string]any{"count": 1},
	}, nil)
	if !result.IsError || !strings.Contains(result.ToolResult, "recovered panic") {
		t.Fatalf("panic result = %+v", result)
	}
}

type forkNilResultTool struct{ *schemaValidationProbeTool }

func (*forkNilResultTool) Name() string { return "ForkNilResult" }
func (*forkNilResultTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return nil, nil
}

func TestExecuteForkToolTreatsNilResultAsFailure(t *testing.T) {
	tool := &forkNilResultTool{schemaValidationProbeTool: &schemaValidationProbeTool{}}
	reg := tools.NewRegistry()
	reg.Register(tool)
	result := executeForkTool(context.Background(), ForkedAgentParams{
		Registry: reg, CanUseTool: AllowAll,
	}, llm.ContentBlock{
		Type: "tool_use", ToolUseID: "fork-nil", ToolName: tool.Name(),
		ToolInput: map[string]any{"count": 1},
	}, nil)
	if !result.IsError || !strings.Contains(result.ToolResult, "no result") {
		t.Fatalf("nil result = %+v, want execution failure", result)
	}
}
