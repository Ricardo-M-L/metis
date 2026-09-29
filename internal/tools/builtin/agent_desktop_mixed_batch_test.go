package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	bashbuiltin "github.com/Ricardo-M-L/metis/internal/tools/builtin/bash"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

type desktopMixedBatchState struct {
	mu          sync.Mutex
	finished    map[string]bool
	preparedIDs map[string]string
	executedIDs map[string]string
	postIDs     map[string]string
	preInputs   map[string]map[string]any
	postInputs  map[string]map[string]any
	wantInputs  map[string]map[string]any
	preCounts   map[string]int
	postCounts  map[string]int
}

// Probe the real shared scheduler while a tool/provider is executing. With
// total capacity one, another acquisition must remain blocked. This catches
// releasing the parent permit around ordinary work in a mixed Agent batch.
func desktopMixedRequireCharged(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 35*time.Millisecond)
	defer cancel()
	release, err := agent.AcquireDesktopSubagentSlot(probeCtx)
	if release != nil {
		release()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("execution did not hold the only Desktop permit: acquisition=%v", err)
	}
	return nil
}

// Forward to the actual filesystem/shell tools. Only observation is wrapped;
// their schemas, admission checks, argument bindings and concurrency are real.
type desktopMixedObservedTool struct {
	tools.Tool
	state *desktopMixedBatchState
}

func (tool desktopMixedObservedTool) CanUse(ctx context.Context, input map[string]any) (tools.Permission, string) {
	tool.state.mu.Lock()
	tool.state.preparedIDs[tool.Name()] = tools.InvocationIDFromContext(ctx)
	tool.state.mu.Unlock()
	return tool.Tool.CanUse(ctx, input)
}

func (tool desktopMixedObservedTool) Execute(ctx context.Context, input map[string]any) (*tools.Result, error) {
	name := tool.Name()
	tool.state.mu.Lock()
	tool.state.executedIDs[name] = tools.InvocationIDFromContext(ctx)
	inputOK := reflect.DeepEqual(input, tool.state.wantInputs[name])
	writeAllowed := name != "Write" || tool.state.finished["Agent"]
	tool.state.mu.Unlock()
	if !inputOK || !writeAllowed {
		return nil, fmt.Errorf("%s lost hook arguments or preceded Agent completion: input=%v", name, input)
	}
	if err := desktopMixedRequireCharged(ctx); err != nil {
		return nil, fmt.Errorf("before %s: %w", name, err)
	}
	result, err := tool.Tool.Execute(ctx, input)
	if err != nil {
		return result, err
	}
	if err := desktopMixedRequireCharged(ctx); err != nil {
		return nil, fmt.Errorf("after %s: %w", name, err)
	}
	tool.state.mu.Lock()
	tool.state.finished[name] = true
	tool.state.mu.Unlock()
	return result, nil
}

type desktopMixedBatchProvider struct {
	llm.Provider
	state    *desktopMixedBatchState
	inputs   map[string]map[string]any
	rootCall atomic.Int32
	children atomic.Int32
}

func (p *desktopMixedBatchProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	if err := desktopCheckToolResults(req); err != nil {
		return nil, err
	}
	depth, _ := ctx.Value(agentDepthKey{}).(int)
	if depth > 0 {
		p.children.Add(1)
		p.state.mu.Lock()
		ordinaryFinished := p.state.finished["Read"] && p.state.finished["Bash"]
		phaseState := fmt.Sprint(p.state.finished)
		p.state.mu.Unlock()
		if !ordinaryFinished {
			return nil, fmt.Errorf("child provider overlapped ordinary tools with total capacity one: finished=%s", phaseState)
		}
		if err := desktopMixedRequireCharged(ctx); err != nil {
			return nil, fmt.Errorf("child provider: %w", err)
		}
		foundPrompt := false
		for _, message := range req.Messages {
			for _, block := range message.Content {
				foundPrompt = foundPrompt || strings.Contains(block.Text, "hook child prompt")
			}
		}
		if !foundPrompt {
			return nil, errors.New("Agent phase lost modified prompt")
		}
		return p.Provider.Stream(ctx, req)
	}
	if p.rootCall.Add(1) == 1 {
		var events []llm.StreamEvent
		// Agent and Exclusive deliberately precede Read/Bash in model order.
		// The dispatcher must execute ordinary tools, then Agent, then Write.
		for _, name := range []string{"Agent", "Write", "Read", "Bash"} {
			input, _ := json.Marshal(p.inputs[name])
			id := "mixed-" + name
			events = append(events,
				llm.StreamEvent{Type: "tool_use_start", ToolUseID: id, ToolName: name},
				llm.StreamEvent{Type: "tool_input_delta", ToolUseID: id, InputDelta: string(input)},
				llm.StreamEvent{Type: "tool_use_stop", ToolUseID: id, InputDelta: string(input)})
		}
		return desktopAgentToolStream(events), nil
	}
	return p.Provider.Stream(ctx, req)
}

func TestAgentDesktopMixedBatchPreservesExecutionChargeHooksAndTrace(t *testing.T) {
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOT_DIR", t.TempDir())
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOTS", "1")
	t.Setenv("METIS_DESKTOP_SUBAGENT_CAP", "1")
	workspace := t.TempDir()
	readPath, writePath := filepath.Join(workspace, "input.txt"), filepath.Join(workspace, "output.txt")
	if err := os.WriteFile(readPath, []byte("real read content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	original := map[string]map[string]any{
		"Read": {"path": readPath}, "Bash": {"command": "printf original", "description": "Print the original fixture value"},
		"Agent": {"prompt": "original child prompt", "name": "mixed-child"},
		"Write": {"path": writePath, "content": "original"},
	}
	state := &desktopMixedBatchState{
		finished: map[string]bool{}, preparedIDs: map[string]string{}, executedIDs: map[string]string{},
		postIDs: map[string]string{}, preInputs: map[string]map[string]any{}, postInputs: map[string]map[string]any{},
		preCounts: map[string]int{}, postCounts: map[string]int{},
		wantInputs: map[string]map[string]any{
			"Read": {"path": readPath, "limit": float64(1)}, "Bash": {"command": "printf hook-bash", "description": "Print the hook rewritten fixture value"},
			"Agent": {"prompt": "hook child prompt", "name": "mixed-child"},
			"Write": {"path": writePath, "content": "hook write\n"},
		},
	}
	gate := permission.New(permission.ModeBypass)
	registry := tools.NewRegistry()
	read := Read{gate: gate, authorizer: newReadPathAuthorizer()}
	write := Write{gate: gate, authorizer: newInvocationAuthorizer[approvedWriteTarget]()}
	bash := bashbuiltin.New(gate, config.ToolBashSettings{Shell: "/bin/sh", TimeoutSeconds: 5, MaxOutputBytes: 1024})
	if bash.Concurrency(state.wantInputs["Bash"]) != tools.ConcurrencySafe || write.Concurrency(nil) != tools.ConcurrencyExclusive {
		t.Fatal("fixture does not exercise ordinary safe + exclusive tools")
	}
	for _, tool := range []tools.Tool{read, bash, write} {
		registry.Register(desktopMixedObservedTool{Tool: tool, state: state})
	}
	roster := agent.NewRoster(4)
	provider := &desktopMixedBatchProvider{Provider: helloProvider(), state: state, inputs: original}
	registry.Register(NewAgent(gate, provider, registry, "mixed-model", "system").WithRoster(roster))
	hooks := agent.NewHookRegistry()
	hooks.Register(agent.PreToolUseHandler(func(_ context.Context, hc agent.HookContext, call *agent.PreToolUseHook) *agent.ModifiedPreToolUse {
		if hc.Model != "mixed-model" {
			t.Errorf("pre-hook model=%q", hc.Model)
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		state.preInputs[call.Tool] = call.Input
		state.preCounts[call.Tool]++
		return &agent.ModifiedPreToolUse{ModifiedInput: state.wantInputs[call.Tool]}
	}))
	hooks.Register(agent.PostToolUseHandler(func(ctx context.Context, hc agent.HookContext, call *agent.PostToolUseHook) {
		if hc.Model != "mixed-model" || call.IsError {
			t.Errorf("post-hook model=%q error=%t output=%s", hc.Model, call.IsError, call.Output)
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		state.postInputs[call.Tool] = call.Input
		state.postCounts[call.Tool]++
		state.postIDs[call.Tool] = pubhook.PostToolUseIDFromContext(ctx)
		if call.Tool == "Agent" {
			state.finished["Agent"] = true
		}
	}))
	loop := agent.NewLoop(provider, registry, gate, hooks, "system", 8)
	loop.Model = "mixed-model"
	loop.AppendUser("run the mixed batch")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer roster.CancelAll()
	events := make(chan agent.Event, 512)
	if err := loop.Run(agent.WithTraceInvocationID(ctx, "mixed-root"), events); err != nil {
		for len(events) > 0 {
			ev := <-events
			if ev.ToolResult != nil {
				t.Logf("tool result %s: %+v", ev.ToolName, ev.ToolResult)
			}
		}
		t.Fatalf("mixed batch failed: %v", err)
	}
	if provider.children.Load() != 1 || !state.finished["Write"] || roster.Count() != 0 {
		t.Fatalf("incomplete phases: child=%d finished=%v live=%d", provider.children.Load(), state.finished, roster.Count())
	}
	if got, err := os.ReadFile(writePath); err != nil || string(got) != "hook write\n" {
		t.Fatalf("real Write did not receive hook input: %q, %v", got, err)
	}
	starts, results := map[string]string{}, map[string]string{}
	startCounts, resultCounts := map[string]int{}, map[string]int{}
	var childTraceIDs []string
	for len(events) > 0 {
		ev := <-events
		switch ev.Kind {
		case agent.EventToolStart:
			starts[ev.ToolUseID] = ev.TraceCallID
			startCounts[ev.ToolUseID]++
		case agent.EventToolResult:
			results[ev.ToolUseID] = ev.TraceCallID
			resultCounts[ev.ToolUseID]++
			if ev.ToolResult.IsError {
				t.Fatalf("tool failed: %+v", ev)
			}
		case agent.EventSubAgentStart, agent.EventSubAgentEnd:
			childTraceIDs = append(childTraceIDs, ev.TraceCallID)
		}
	}
	for name, input := range original {
		id := "mixed-" + name
		if !reflect.DeepEqual(state.preInputs[name], input) || !reflect.DeepEqual(state.postInputs[name], state.wantInputs[name]) || state.postIDs[name] != id {
			t.Fatalf("hook contract changed for %s: pre=%v post=%v postID=%s", name, state.preInputs[name], state.postInputs[name], state.postIDs[name])
		}
		if state.preCounts[name] != 1 || state.postCounts[name] != 1 || startCounts[id] != 1 || resultCounts[id] != 1 {
			t.Fatalf("%s duplicated or dropped hooks/events: pre=%d post=%d start=%d result=%d", name, state.preCounts[name], state.postCounts[name], startCounts[id], resultCounts[id])
		}
		if starts[id] == "" || results[id] != starts[id] {
			t.Fatalf("tool start/result correlation lost for %s: starts=%v results=%v", name, starts, results)
		}
		if name != "Agent" && (state.preparedIDs[name] != starts[id] || state.executedIDs[name] != starts[id]) {
			t.Fatalf("permission/execution occurrence changed for %s", name)
		}
	}
	if len(childTraceIDs) != 3 {
		t.Fatalf("child lifecycle count=%d", len(childTraceIDs))
	}
	for _, traceID := range childTraceIDs {
		if traceID != starts["mixed-Agent"] {
			t.Fatalf("child detached from Agent occurrence: %q", traceID)
		}
	}
	for _, message := range loop.History() {
		for _, block := range message.Content {
			if block.Type == "tool_use" || block.Type == "tool_result" {
				if block.TraceCallID == "" || block.TraceCallID != starts[block.ToolUseID] {
					t.Fatalf("durable tool occurrence mismatch: %+v", block)
				}
			}
		}
	}
}
