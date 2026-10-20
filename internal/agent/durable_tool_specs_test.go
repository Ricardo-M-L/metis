package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

type durableSchemaTestTool struct {
	fakeTool
	schema map[string]any
}

func (t durableSchemaTestTool) Description() string {
	return "Execute a shell command with scoped cwd and environment.\n\nSafety:\nPreserve user files and follow the permission policy.\n\nLong-running commands: MUST set run_in_background=true.\nFinite acceptance checks: use await_completion=true and required_for_completion=true."
}
func (t durableSchemaTestTool) ShortDescription() string {
	return "Execute commands; finite checks require await_completion=true, and long commands require run_in_background=true."
}
func (t durableSchemaTestTool) InputSchema() map[string]any { return t.schema }

func durableToolSpecsFixture(t *testing.T) (*Loop, map[string]any) {
	t.Helper()
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":                 map[string]any{"type": "string", "description": "Command to execute"},
			"description":             map[string]any{"type": "string"},
			"timeout_ms":              map[string]any{"type": "integer", "description": "Use run_in_background=true for longer work", "default": 120000},
			"run_in_background":       map[string]any{"type": "boolean", "default": false},
			"await_completion":        map[string]any{"type": "boolean", "default": false},
			"required_for_completion": map[string]any{"type": "boolean", "default": false},
		},
		"required": []string{"command", "description", "await_completion"},
		"default":  map[string]any{"await_completion": true, "description": "unchanged", "metadata": []any{map[string]any{"nested": "original"}}},
	}
	registry := tools.NewRegistry()
	registry.Register(&durableSchemaTestTool{fakeTool: fakeTool{name: "Bash"}, schema: schema})
	registry.Register(&fakeTool{name: "Read"})
	registry.Register(&fakeTool{name: "mcp__remote__write"})
	return &Loop{Registry: registry}, schema
}

func findDurableTestSpec(t *testing.T, specs []llm.ToolSpec, name string) llm.ToolSpec {
	t.Helper()
	for _, spec := range specs {
		if spec.Name == name {
			return spec
		}
	}
	t.Fatalf("tool %s missing from projected specs", name)
	return llm.ToolSpec{}
}

func TestDurableToolSpecsPublishForegroundContractWithoutMutatingInteractiveSchema(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	for _, short := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "short"}[short], func(t *testing.T) {
			loop, original := durableToolSpecsFixture(t)
			loop.ShortToolDescriptions = short
			originalJSON, _ := json.Marshal(original)
			ctx := pubtool.WithOperationExecutor(context.Background(), &recordingOperationExecutor{})
			durable := findDurableTestSpec(t, loop.toolSpecsWithContext(ctx), "Bash")
			properties := durable.InputSchema["properties"].(map[string]any)
			for _, field := range durableBashBackgroundFields {
				if _, advertised := properties[field]; advertised {
					t.Fatalf("durable provider spec advertises forbidden background flag %s", field)
				}
			}
			if properties["timeout_ms"] == nil || !strings.Contains(durable.Description, "foreground") || !strings.Contains(durable.Description, "Safety:") {
				t.Fatalf("foreground timeout or ordinary safety guidance was lost: %+v", durable)
			}
			encoded, _ := json.Marshal(durable)
			for _, advice := range []string{"run_in_background=true", "await_completion=true", "required_for_completion=true"} {
				if strings.Contains(string(encoded), advice) {
					t.Fatalf("provider spec still advises incompatible background execution: %s", advice)
				}
			}
			if !reflect.DeepEqual(durable.InputSchema["required"], []string{"command", "description"}) {
				t.Fatalf("removed background property remained required: %+v", durable.InputSchema["required"])
			}
			defaults := durable.InputSchema["default"].(map[string]any)
			if _, advertised := defaults["await_completion"]; advertised {
				t.Fatal("removed background property remained in object defaults")
			}
			properties["command"].(map[string]any)["description"] = "mutated copy"
			defaults["metadata"].([]any)[0].(map[string]any)["nested"] = "mutated copy"
			afterJSON, _ := json.Marshal(original)
			if string(originalJSON) != string(afterJSON) {
				t.Fatal("durable provider spec rewrote a shared registry schema/default")
			}
			interactive := findDurableTestSpec(t, loop.toolSpecsWithContext(context.Background()), "Bash")
			for _, field := range durableBashBackgroundFields {
				if interactive.InputSchema["properties"].(map[string]any)[field] == nil {
					t.Fatalf("interactive Bash lost background property %s", field)
				}
			}
		})
	}
}

func TestDurableToolSpecsPreserveLazyToolsAndDetachedContext(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "true")
	loop, _ := durableToolSpecsFixture(t)
	ctx, cancel := context.WithCancel(pubtool.WithOperationExecutor(context.Background(), &recordingOperationExecutor{}))
	cancel()
	ordinary := loop.toolSpecs()
	durable := loop.toolSpecsWithContext(context.WithoutCancel(ctx))
	if len(durable) != len(ordinary) {
		t.Fatal("durable projection dropped lazy catalog entries")
	}
	for _, name := range []string{"Read", "mcp__remote__write", "ToolSearch"} {
		before := findDurableTestSpec(t, ordinary, name)
		after := findDurableTestSpec(t, durable, name)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("durable projection changed unrelated/lazy tool %s", name)
		}
	}
	if !isLazyPlaceholderSpec(findDurableTestSpec(t, durable, "mcp__remote__write")) {
		t.Fatal("lazy schema discovery was bypassed")
	}
	properties := findDurableTestSpec(t, durable, "Bash").InputSchema["properties"].(map[string]any)
	if _, exists := properties["await_completion"]; exists {
		t.Fatal("detached child lost its durable schema contract")
	}
}

func TestDurableToolSpecsFilterJSONRequiredArrays(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	loop, original := durableToolSpecsFixture(t)
	original["required"] = []any{"command", "run_in_background", "description", "required_for_completion"}
	ctx := pubtool.WithOperationExecutor(context.Background(), &recordingOperationExecutor{})
	projected := findDurableTestSpec(t, loop.toolSpecsWithContext(ctx), "Bash")
	if !reflect.DeepEqual(projected.InputSchema["required"], []any{"command", "description"}) {
		t.Fatalf("JSON required array still contains background flags: %+v", projected.InputSchema["required"])
	}
	if len(original["required"].([]any)) != 4 {
		t.Fatal("required filtering mutated the shared schema")
	}
}

func TestDurableToolSpecsReachInitialAndRebuiltProviderRequests(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "true")
	for _, guarded := range []bool{false, true} {
		t.Run(map[bool]string{false: "interactive", true: "durable"}[guarded], func(t *testing.T) {
			fixture, _ := durableToolSpecsFixture(t)
			provider := &queuedStreamProvider{streams: []llm.StreamReader{toolUseStream("read-once", "Read", `{}`), textStream("Read finished; no state was modified.")}}
			loop := NewLoop(provider, fixture.Registry, permission.New(permission.ModeBypassPermissions), nil, "system", 4)
			loop.ShortToolDescriptions = true
			loop.AppendUser("Read once and report the result")
			ctx := context.Background()
			if guarded {
				ctx = pubtool.WithOperationExecutor(ctx, &recordingOperationExecutor{})
			}
			if err := loop.Run(ctx, make(chan Event, 128)); err != nil {
				t.Fatal(err)
			}
			requests := provider.capturedRequests()
			if len(requests) != 2 {
				t.Fatalf("expected initial and post-tool rebuilt requests, got %d", len(requests))
			}
			for _, request := range requests {
				bash := findDurableTestSpec(t, request.Tools, "Bash")
				properties := bash.InputSchema["properties"].(map[string]any)
				for _, field := range durableBashBackgroundFields {
					_, advertised := properties[field]
					if advertised == guarded {
						t.Fatalf("actual provider request has wrong background contract: guarded=%v field=%s", guarded, field)
					}
				}
				if guarded && strings.Contains(bash.Description, "await_completion=true") {
					t.Fatal("actual durable provider request still advises background completion")
				}
				findDurableTestSpec(t, request.Tools, "ToolSearch")
			}
		})
	}
}
