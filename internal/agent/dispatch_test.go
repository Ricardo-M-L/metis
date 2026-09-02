package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

// orderRecorder is a goroutine-safe append-only string log used by
// dispatch tests that care about start order across the
// safe/queue/exclusive phases.
type orderRecorder struct {
	mu  sync.Mutex
	log []string
}

func (o *orderRecorder) Append(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.log = append(o.log, name)
}

func (o *orderRecorder) Snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, len(o.log))
	copy(out, o.log)
	return out
}

// fakeTool is a minimal tools.Tool for dispatch tests. Each instance
// records when it started/finished and reports its concurrency tier so
// the test can verify the dispatcher's phasing.
type fakeTool struct {
	name        string
	conc        tools.Concurrency
	starts      *atomic.Int64
	finishes    *atomic.Int64
	concurrent  *atomic.Int64
	maxConc     *atomic.Int64
	hold        time.Duration
	startsOrder *orderRecorder
}

type presentationTool struct {
	fakeTool
	presentation map[string]any
}

type hiddenDispatchTool struct{ *fakeTool }

func (hiddenDispatchTool) ToolExposure() tools.ToolExposure { return tools.ToolExposureHidden }

type deferredDispatchTool struct{ *fakeTool }

func (deferredDispatchTool) ToolExposure() tools.ToolExposure { return tools.ToolExposureDeferred }

type readOnlyDispatchTool struct{ *fakeTool }

func (readOnlyDispatchTool) IsReadOnly(map[string]any) bool { return true }

type bypassPermissionProbeTool struct {
	fakeTool
	permission  tools.Permission
	reason      string
	interactive bool
	autoAllow   bool
}

func (t *bypassPermissionProbeTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return t.permission, t.reason
}

func (t *bypassPermissionProbeTool) RequiresUserInteraction() bool {
	return t.interactive
}

func (t *bypassPermissionProbeTool) CanAutoAllowInBypass(map[string]any) bool {
	return t.autoAllow
}

type legacyApprovalTool struct {
	fakeTool
}

type permissionInputCaptureTool struct {
	fakeTool
	seen chan map[string]any
}

type invocationIDProbeTool struct {
	fakeTool
	canUseID  string
	executeID string
}

func (t *invocationIDProbeTool) CanUse(ctx context.Context, _ map[string]any) (tools.Permission, string) {
	t.canUseID = tools.InvocationIDFromContext(ctx)
	return tools.PermissionAllow, ""
}

func (t *invocationIDProbeTool) Execute(ctx context.Context, _ map[string]any) (*tools.Result, error) {
	t.executeID = tools.InvocationIDFromContext(ctx)
	return &tools.Result{Output: "ok"}, nil
}

func (t *permissionInputCaptureTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAsk, "approval required"
}

func (t *permissionInputCaptureTool) Execute(_ context.Context, input map[string]any) (*tools.Result, error) {
	t.seen <- input
	return &tools.Result{Output: "ok"}, nil
}

func (t *legacyApprovalTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAsk, "legacy plugin requires approval"
}

func TestDetachInterruptBlockContextPreservesTurnDetachAndHardLifecycle(t *testing.T) {
	t.Run("top-level turn cancellation stays detached", func(t *testing.T) {
		turnCtx, cancelTurn := context.WithCancel(context.Background())
		detached, cleanup := detachInterruptBlockContext(turnCtx)
		defer cleanup()
		cancelTurn()

		select {
		case <-detached.Done():
			t.Fatalf("top-level InterruptBlock context inherited ordinary turn cancellation: %v", detached.Err())
		case <-time.After(20 * time.Millisecond):
		}
	})

	t.Run("sub-agent hard lifecycle remains attached", func(t *testing.T) {
		turnCtx, cancelTurn := context.WithCancel(context.Background())
		hardCtx, cancelHard := context.WithCancel(context.Background())
		stamped := WithToolLifecycleContext(turnCtx, hardCtx)
		detached, cleanup := detachInterruptBlockContext(stamped)
		defer cleanup()

		cancelTurn()
		select {
		case <-detached.Done():
			t.Fatalf("ordinary turn cancellation crossed the InterruptBlock boundary: %v", detached.Err())
		case <-time.After(20 * time.Millisecond):
		}

		cancelHard()
		select {
		case <-detached.Done():
			if !errors.Is(detached.Err(), context.Canceled) {
				t.Fatalf("hard lifecycle error = %v, want context.Canceled", detached.Err())
			}
		case <-time.After(time.Second):
			t.Fatal("hard lifecycle cancellation did not reach InterruptBlock context")
		}
	})

	t.Run("already-cancelled hard lifecycle fails closed", func(t *testing.T) {
		hardCtx, cancelHard := context.WithCancel(context.Background())
		cancelHard()
		detached, cleanup := detachInterruptBlockContext(
			WithToolLifecycleContext(context.Background(), hardCtx),
		)
		defer cleanup()
		select {
		case <-detached.Done():
		case <-time.After(time.Second):
			t.Fatal("pre-cancelled hard lifecycle was lost during detach")
		}
	})
}

func (f *presentationTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{
		Output:       "Artifact updated",
		Display:      "Interactive artifact",
		Presentation: f.presentation,
	}, nil
}

func (f *fakeTool) Name() string                                 { return f.name }
func (f *fakeTool) Description() string                          { return "test" }
func (f *fakeTool) InputSchema() map[string]any                  { return map[string]any{"type": "object"} }
func (f *fakeTool) Concurrency(map[string]any) tools.Concurrency { return f.conc }
func (f *fakeTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (f *fakeTool) Execute(ctx context.Context, _ map[string]any) (*tools.Result, error) {
	if f.starts != nil {
		f.starts.Add(1)
	}
	if f.startsOrder != nil {
		f.startsOrder.Append(f.name)
	}
	if f.concurrent != nil {
		now := f.concurrent.Add(1)
		for {
			cur := f.maxConc.Load()
			if now <= cur || f.maxConc.CompareAndSwap(cur, now) {
				break
			}
		}
	}
	time.Sleep(f.hold)
	if f.concurrent != nil {
		f.concurrent.Add(-1)
	}
	if f.finishes != nil {
		f.finishes.Add(1)
	}
	return &tools.Result{Output: f.name}, nil
}

func TestDispatchPassesSameInvocationIDToCanUseAndExecute(t *testing.T) {
	probe := &invocationIDProbeTool{fakeTool: fakeTool{name: "InvocationProbe", conc: tools.ConcurrencyExclusive}}
	reg := tools.NewRegistry()
	reg.Register(probe)
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}
	if _, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "provider-id-may-repeat", ToolName: probe.Name(),
	}}, make(chan Event, 8), HookContext{}); err != nil {
		t.Fatal(err)
	}
	if probe.canUseID == "" || probe.executeID == "" || probe.canUseID != probe.executeID {
		t.Fatalf("CanUse invocation=%q Execute invocation=%q, want same non-empty ID", probe.canUseID, probe.executeID)
	}
}

func TestDispatch_BypassAutoAllowsOrdinaryToolAskWithoutPermissionEvent(t *testing.T) {
	started := &atomic.Int64{}
	reg := tools.NewRegistry()
	reg.Register(&bypassPermissionProbeTool{
		fakeTool:   fakeTool{name: "PluginAction", conc: tools.ConcurrencyExclusive, starts: started},
		permission: tools.PermissionAsk,
		reason:     "plugin normally asks",
		autoAllow:  true,
	})
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}
	out := make(chan Event, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	results, err := loop.executeBatch(ctx, []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "plugin-ask", ToolName: "PluginAction",
	}}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if started.Load() != 1 {
		t.Fatalf("ordinary ASK tool executions = %d, want 1 in bypassPermissions", started.Load())
	}
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("results = %+v, want successful tool result", results)
	}
	for len(out) > 0 {
		ev := <-out
		if ev.Kind == EventPermissionRequest || ev.Kind == EventAskUser {
			t.Fatalf("bypass emitted interactive event %v", ev.Kind)
		}
	}
}

func TestDispatch_BypassSilentlyDeniesLegacyPluginAskWithoutOptIn(t *testing.T) {
	started := &atomic.Int64{}
	reg := tools.NewRegistry()
	reg.Register(&legacyApprovalTool{fakeTool: fakeTool{
		name: "LegacySensitivePlugin", conc: tools.ConcurrencyExclusive, starts: started,
	}})
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}
	out := make(chan Event, 16)
	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "legacy-ask", ToolName: "LegacySensitivePlugin",
	}}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if started.Load() != 0 {
		t.Fatalf("legacy approval tool executed %d times, want 0", started.Load())
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, "not opted") {
		t.Fatalf("results = %+v, want structured fail-closed result", results)
	}
	for len(out) > 0 {
		ev := <-out
		if ev.Kind == EventPermissionRequest || ev.Kind == EventAskUser {
			t.Fatalf("bypass emitted interactive event %v", ev.Kind)
		}
	}
}

func TestDispatch_FullAccessRunsLegacyPluginAskWithoutPermissionEvent(t *testing.T) {
	started := &atomic.Int64{}
	reg := tools.NewRegistry()
	reg.Register(&legacyApprovalTool{fakeTool: fakeTool{
		name: "LegacySensitivePlugin", conc: tools.ConcurrencyExclusive, starts: started,
	}})
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeFullAccess)}
	out := make(chan Event, 16)
	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "legacy-full-access", ToolName: "LegacySensitivePlugin",
	}}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if started.Load() != 1 || len(results) != 1 || results[0].IsError {
		t.Fatalf("fullAccess legacy ASK execution: starts=%d results=%+v", started.Load(), results)
	}
	for len(out) > 0 {
		ev := <-out
		if ev.Kind == EventPermissionRequest {
			t.Fatalf("fullAccess emitted approval event: %+v", ev)
		}
	}
}

func TestDispatch_BypassSilentlyDeniesInteractionRequiredTool(t *testing.T) {
	started := &atomic.Int64{}
	reg := tools.NewRegistry()
	reg.Register(&bypassPermissionProbeTool{
		fakeTool:    fakeTool{name: "OAuthPrompt", conc: tools.ConcurrencyExclusive, starts: started},
		permission:  tools.PermissionAllow,
		interactive: true,
	})
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeBypassPermissions)}
	out := make(chan Event, 16)

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "interactive", ToolName: "OAuthPrompt",
	}}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if started.Load() != 0 {
		t.Fatalf("interaction-required tool executed %d times, want 0", started.Load())
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, "unavailable") {
		t.Fatalf("results = %+v, want structured interaction-unavailable error", results)
	}
	for len(out) > 0 {
		ev := <-out
		if ev.Kind == EventPermissionRequest || ev.Kind == EventAskUser {
			t.Fatalf("bypass emitted interactive event %v", ev.Kind)
		}
	}
}

func TestDispatch_BypassPlanLineageStillDeniesInteractionRequiredTool(t *testing.T) {
	started := &atomic.Int64{}
	reg := tools.NewRegistry()
	reg.Register(&bypassPermissionProbeTool{
		fakeTool:    fakeTool{name: "OAuthPrompt", conc: tools.ConcurrencyExclusive, starts: started},
		permission:  tools.PermissionAllow,
		interactive: true,
	})
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModePlan)}
	loop.SetPrePlanMode(string(permission.ModeBypassPermissions))
	out := make(chan Event, 16)

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "interactive-plan-lineage", ToolName: "OAuthPrompt",
	}}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if started.Load() != 0 {
		t.Fatalf("interaction-required tool executed %d times, want 0", started.Load())
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, "unavailable") {
		t.Fatalf("results = %+v, want structured interaction-unavailable error", results)
	}
	for len(out) > 0 {
		ev := <-out
		if ev.Kind == EventPermissionRequest || ev.Kind == EventAskUser {
			t.Fatalf("bypass plan lineage emitted interactive event %v", ev.Kind)
		}
	}
}

func TestDispatch_BypassPlanLineageDoesNotUpgradeBypassImmuneAsk(t *testing.T) {
	started := &atomic.Int64{}
	reg := tools.NewRegistry()
	reg.Register(&bypassPermissionProbeTool{
		fakeTool:   fakeTool{name: "CredentialRead", conc: tools.ConcurrencyExclusive, starts: started},
		permission: tools.PermissionAsk,
		reason:     "secret_read:bypass_immune",
	})
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModePlan)}
	loop.SetPrePlanMode(string(permission.ModeBypassPermissions))
	out := make(chan Event, 16)

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "credential-plan-lineage", ToolName: "CredentialRead",
	}}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if started.Load() != 0 {
		t.Fatalf("bypass-immune tool executed %d times, want 0", started.Load())
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, "bypass_immune") {
		t.Fatalf("results = %+v, want structured bypass-immune denial", results)
	}
	for len(out) > 0 {
		ev := <-out
		if ev.Kind == EventPermissionRequest || ev.Kind == EventAskUser {
			t.Fatalf("bypass plan lineage emitted interactive event %v", ev.Kind)
		}
	}
}

func TestDispatchHookCredentialInputExecutesRawButPermissionEventsAreRedacted(t *testing.T) {
	const secret = "hook-injected-secret-without-known-prefix"
	modifiedInput := map[string]any{
		"api_key": secret,
		"target":  "prod",
		"nested":  map[string]any{"region": "east"},
	}
	seen := make(chan map[string]any, 1)
	reg := tools.NewRegistry()
	reg.Register(&permissionInputCaptureTool{
		fakeTool: fakeTool{name: "Deploy", conc: tools.ConcurrencyExclusive},
		seen:     seen,
	})
	hooks := pubhook.NewRegistry()
	hooks.Register(pubhook.PreToolUseHandler(func(context.Context, pubhook.Context, *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
		return &pubhook.ModifiedPreToolUse{
			ModifiedInput: modifiedInput,
			PresentationInput: map[string]any{
				// A third-party hook may forget to redact its presentation copy.
				// Dispatch must still enforce the final UI/persistence boundary.
				"api_key": secret,
				"target":  "prod",
				"nested":  map[string]any{"region": "east"},
			},
		}
	}))
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeDefault), Hooks: hooks}
	events := make(chan Event, 16)
	done := make(chan error, 1)
	go func() {
		_, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "deploy-1", ToolName: "Deploy",
			ToolInput: map[string]any{"target": "prod"},
		}}, events, HookContext{})
		done <- err
	}()

	var permissionEvent Event
	deadline := time.After(2 * time.Second)
	for permissionEvent.Kind != EventPermissionRequest {
		select {
		case ev := <-events:
			if strings.Contains(fmt.Sprint(ev.ToolInput), secret) || strings.Contains(fmt.Sprint(ev.PermissionInput), secret) {
				t.Fatalf("event leaked hook-injected credential: %+v", ev)
			}
			if ev.Kind == EventPermissionRequest {
				permissionEvent = ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for permission event")
		}
	}
	if got := fmt.Sprint(permissionEvent.PermissionInput["api_key"]); got != "[REDACTED]" {
		t.Fatalf("permission presentation api_key = %q, want redacted", got)
	}
	if got := fmt.Sprint(permissionEvent.ToolInput["api_key"]); got != "[REDACTED]" {
		t.Fatalf("tool presentation api_key = %q, want redacted", got)
	}
	if formatted := fmt.Sprintf("%+v", permissionEvent); strings.Contains(formatted, secret) {
		t.Fatalf("generic event formatting leaked private policy input: %s", formatted)
	}
	policyInput, ok := permissionEvent.PermissionPolicyInputForAuthorization()
	if !ok || policyInput["api_key"] != secret {
		t.Fatalf("policy input = %#v, %v; want exact execution credential", policyInput, ok)
	}
	policyInput["api_key"] = "mutated-consumer-copy"
	policyInput["nested"].(map[string]any)["region"] = "west"
	policyAgain, ok := permissionEvent.PermissionPolicyInputForAuthorization()
	if !ok || policyAgain["api_key"] != secret || policyAgain["nested"].(map[string]any)["region"] != "east" {
		t.Fatalf("policy accessor did not return a defensive deep copy: %#v, %v", policyAgain, ok)
	}
	permissionEvent.PermissionReply <- PermissionDecisionAllow
	if err := <-done; err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	select {
	case raw := <-seen:
		if raw["api_key"] != secret {
			t.Fatalf("execution input lost credential: %#v", raw)
		}
	default:
		t.Fatal("tool did not execute after approval")
	}
	modifiedInput["api_key"] = "mutated-hook-map"
	modifiedInput["nested"].(map[string]any)["region"] = "south"
	policyAfterMutation, ok := permissionEvent.PermissionPolicyInputForAuthorization()
	if !ok || policyAfterMutation["api_key"] != secret || policyAfterMutation["nested"].(map[string]any)["region"] != "east" {
		t.Fatalf("permission event policy snapshot aliased hook input: %#v, %v", policyAfterMutation, ok)
	}
}

func TestDispatchAdmissionChecksFinalRawInputBeforeCanUseAllow(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	const secret = "hunter2"
	started := &atomic.Int64{}
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "mcp__test__mutate", conc: tools.ConcurrencyExclusive, starts: started,
	})
	hooks := pubhook.NewRegistry()
	hooks.Register(pubhook.PreToolUseHandler(func(context.Context, pubhook.Context, *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
		return &pubhook.ModifiedPreToolUse{ModifiedInput: map[string]any{
			"password":  secret,
			"operation": "hook-mutated",
		}}
	}))
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeDefault), Hooks: hooks}
	var policyInput map[string]any
	ctx := WithToolAdmissionPolicy(context.Background(), func(tool string, input map[string]any) (bool, string) {
		if tool != "mcp__test__mutate" {
			t.Fatalf("admission tool = %q", tool)
		}
		policyInput = input
		input["operation"] = "mutated-by-policy"
		return false, "unauthorized"
	})
	out := make(chan Event, 16)
	results, err := loop.executeBatch(ctx, []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "mcp-admission", ToolName: "mcp__test__mutate",
		ToolInput: map[string]any{"operation": "model-original"},
	}}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if started.Load() != 0 {
		t.Fatal("CanUse=ALLOW side-effect tool executed despite admission denial")
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, "unattended admission") {
		t.Fatalf("results = %+v, want unattended admission denial", results)
	}
	if policyInput["password"] != secret || policyInput["operation"] != "mutated-by-policy" {
		t.Fatalf("policy input = %#v, want exact hook-mutated raw snapshot", policyInput)
	}

	var request Event
	for len(out) > 0 {
		ev := <-out
		if ev.Kind == EventPermissionRequest {
			request = ev
		}
	}
	if request.Kind != EventPermissionRequest {
		t.Fatal("missing admission denial permission event")
	}
	if got := request.PermissionInput["password"]; got != "[REDACTED]" {
		t.Fatalf("permission presentation password = %#v, want redacted", got)
	}
	raw, ok := request.PermissionPolicyInputForAuthorization()
	if !ok || raw["password"] != secret || raw["operation"] != "hook-mutated" {
		t.Fatalf("private policy input = %#v, %v; want immutable final raw input", raw, ok)
	}
}

func TestDispatchAdmissionSkipsDeclaredReadOnlyTools(t *testing.T) {
	started := &atomic.Int64{}
	reg := tools.NewRegistry()
	reg.Register(readOnlyDispatchTool{&fakeTool{
		name: "ReadOnlyLookup", conc: tools.ConcurrencySafe, starts: started,
	}})
	loop := &Loop{Registry: reg, Gate: permission.New(permission.ModeDefault)}
	policyCalls := 0
	ctx := WithToolAdmissionPolicy(context.Background(), func(string, map[string]any) (bool, string) {
		policyCalls++
		return false, "unauthorized"
	})
	results, err := loop.executeBatch(ctx, []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "read-only", ToolName: "ReadOnlyLookup",
	}}, make(chan Event, 8), HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if policyCalls != 0 || started.Load() != 1 || len(results) != 1 || results[0].IsError {
		t.Fatalf("read-only call: policy=%d starts=%d results=%+v", policyCalls, started.Load(), results)
	}
}

func TestRedactedToolInputMasksCredentialNamedSubtreesRecursively(t *testing.T) {
	original := map[string]any{
		"password": "hunter2",
		"nested": map[string]any{
			"apiKey": map[string]any{"value": "opaque-api-secret"},
		},
		"items": []any{map[string]any{
			"auth": map[string]any{"value": "opaque-auth-secret"},
		}},
		"labels": map[string]string{
			"client-token": "opaque-client-token",
			"note":         "keep",
		},
		"ordinary": "hunter2",
		"command":  "PASSWORD=command-secret run",
	}
	got := redactedToolInput(original)
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"opaque-api-secret", "opaque-auth-secret", "opaque-client-token", "command-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("redacted input leaked %q: %s", secret, encoded)
		}
	}
	if got["password"] != "[REDACTED]" || got["ordinary"] != "hunter2" {
		t.Fatalf("top-level structured redaction = %#v", got)
	}
	if got["nested"].(map[string]any)["apiKey"] != "[REDACTED]" {
		t.Fatalf("nested apiKey subtree was not replaced: %#v", got["nested"])
	}
	if got["items"].([]any)[0].(map[string]any)["auth"] != "[REDACTED]" {
		t.Fatalf("auth subtree was not replaced: %#v", got["items"])
	}
	if got["labels"].(map[string]string)["client-token"] != "[REDACTED]" ||
		got["labels"].(map[string]string)["note"] != "keep" {
		t.Fatalf("map[string]string redaction = %#v", got["labels"])
	}
	if original["password"] != "hunter2" || original["nested"].(map[string]any)["apiKey"].(map[string]any)["value"] != "opaque-api-secret" {
		t.Fatalf("redaction mutated execution input: %#v", original)
	}
}

// TestDispatch_QueueSerializesAmongItself verifies that two queue-tier
// tools in the same batch run sequentially, never overlapping each
// other — even though the safe tools running alongside them DO fan out.
func TestDispatch_QueueSerializesAmongItself(t *testing.T) {
	queueConc := &atomic.Int64{}
	queueMax := &atomic.Int64{}

	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "Q1", conc: tools.ConcurrencyQueue,
		concurrent: queueConc, maxConc: queueMax,
		hold: 30 * time.Millisecond,
	})
	reg.Register(&fakeTool{
		name: "Q2", conc: tools.ConcurrencyQueue,
		concurrent: queueConc, maxConc: queueMax,
		hold: 30 * time.Millisecond,
	})

	loop := &Loop{Registry: reg}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "1", ToolName: "Q1"},
		{Type: "tool_use", ToolUseID: "2", ToolName: "Q2"},
	}
	out := make(chan Event, 32)
	if _, err := loop.executeBatch(context.Background(), uses, out, HookContext{}); err != nil {
		t.Fatalf("executeBatch err: %v", err)
	}
	if got := queueMax.Load(); got != 1 {
		t.Errorf("queue tools should be FIFO (max concurrent=1); got %d", got)
	}
}

// TestDispatch_QueueRunsAlongsideSafe verifies that queue and safe
// tools run concurrently — the queue tier doesn't block fanout, just
// serializes within itself.
func TestDispatch_QueueRunsAlongsideSafe(t *testing.T) {
	overall := &atomic.Int64{}
	overallMax := &atomic.Int64{}

	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "Safe1", conc: tools.ConcurrencySafe,
		concurrent: overall, maxConc: overallMax, hold: 30 * time.Millisecond,
	})
	reg.Register(&fakeTool{
		name: "Safe2", conc: tools.ConcurrencySafe,
		concurrent: overall, maxConc: overallMax, hold: 30 * time.Millisecond,
	})
	reg.Register(&fakeTool{
		name: "Q1", conc: tools.ConcurrencyQueue,
		concurrent: overall, maxConc: overallMax, hold: 30 * time.Millisecond,
	})

	loop := &Loop{Registry: reg}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "1", ToolName: "Safe1"},
		{Type: "tool_use", ToolUseID: "2", ToolName: "Safe2"},
		{Type: "tool_use", ToolUseID: "3", ToolName: "Q1"},
	}
	out := make(chan Event, 32)
	if _, err := loop.executeBatch(context.Background(), uses, out, HookContext{}); err != nil {
		t.Fatalf("executeBatch err: %v", err)
	}
	// At least 2 tools should have been concurrent (the two safe ones,
	// possibly with the queue running alongside).
	if got := overallMax.Load(); got < 2 {
		t.Errorf("safe tools should fan out (max concurrent>=2); got %d", got)
	}
}

// TestDispatch_ExclusiveRunsAfterAll verifies the existing exclusive
// tier still serializes AFTER the parallel + queue work — the new
// queue tier didn't perturb it.
func TestDispatch_ExclusiveRunsAfterAll(t *testing.T) {
	rec := &orderRecorder{}

	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "Safe1", conc: tools.ConcurrencySafe,
		startsOrder: rec, hold: 10 * time.Millisecond,
	})
	reg.Register(&fakeTool{
		name: "Q1", conc: tools.ConcurrencyQueue,
		startsOrder: rec, hold: 10 * time.Millisecond,
	})
	reg.Register(&fakeTool{
		name: "Excl1", conc: tools.ConcurrencyExclusive,
		startsOrder: rec, hold: 5 * time.Millisecond,
	})

	loop := &Loop{Registry: reg}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "1", ToolName: "Safe1"},
		{Type: "tool_use", ToolUseID: "2", ToolName: "Q1"},
		{Type: "tool_use", ToolUseID: "3", ToolName: "Excl1"},
	}
	out := make(chan Event, 32)
	if _, err := loop.executeBatch(context.Background(), uses, out, HookContext{}); err != nil {
		t.Fatalf("executeBatch err: %v", err)
	}
	order := rec.Snapshot()
	if len(order) != 3 {
		t.Fatalf("want 3 starts, got %d", len(order))
	}
	if order[len(order)-1] != "Excl1" {
		t.Errorf("Excl1 should start last; order=%v", order)
	}
}

func TestDispatch_PreservesStructuredPresentationForLiveAndReplay(t *testing.T) {
	presentation := map[string]any{
		"kind": "artifact",
		"artifact": map[string]any{
			"id":      "art-123",
			"version": 2,
		},
		"actions": []any{
			map[string]any{"id": "open", "label": "Open"},
		},
	}
	reg := tools.NewRegistry()
	reg.Register(&presentationTool{
		fakeTool:     fakeTool{name: "Artifact", conc: tools.ConcurrencySafe},
		presentation: presentation,
	})
	loop := &Loop{Registry: reg}
	out := make(chan Event, 8)
	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "tu-artifact", ToolName: "Artifact",
	}}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	block := results[0]
	blockArtifact, ok := block.Presentation["artifact"].(map[string]any)
	if !ok || block.Display != "Interactive artifact" || blockArtifact["id"] != "art-123" {
		t.Fatalf("persisted block lost presentation metadata: %+v", block)
	}

	var live *ToolResult
	for len(out) > 0 {
		ev := <-out
		if ev.Kind == EventToolResult {
			live = ev.ToolResult
		}
	}
	if live == nil {
		t.Fatal("missing live EventToolResult")
	}
	liveArtifact, ok := live.Presentation["artifact"].(map[string]any)
	if !ok || live.Display != "Interactive artifact" || liveArtifact["version"] != 2 {
		t.Fatalf("live event lost presentation metadata: %+v", live)
	}

	// Dispatch owns the two outbound maps: later tool or live-renderer
	// mutation must not rewrite persisted history (or vice versa).
	presentation["artifact"].(map[string]any)["version"] = 3
	liveArtifact["version"] = 4
	live.Presentation["actions"].([]any)[0].(map[string]any)["label"] = "Launch"
	if got := blockArtifact["version"]; got != 2 {
		t.Fatalf("persisted presentation aliased another owner: version = %#v", got)
	}
	blockActions := block.Presentation["actions"].([]any)
	if got := blockActions[0].(map[string]any)["label"]; got != "Open" {
		t.Fatalf("persisted action aliased live event: label = %#v", got)
	}
}

// TestShortToolDesc — first paragraph wins, falls back to single-line,
// hard cap at 200 chars. Mirrors Crush's CRUSH_SHORT_TOOL_DESCRIPTIONS
// behaviour: a tool's full doc may be useful for `metis tools` listing
// but the LLM only needs enough to pick the tool.
func TestShortToolDesc(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"single line", "Read a file.", "Read a file."},
		{
			"paragraph break",
			"Read a file from disk.\n\nThis tool returns numbered lines.\n\nWhen the file is binary…",
			"Read a file from disk.",
		},
		{
			"single newline only",
			"Run a shell command.\nOutput is captured and truncated.",
			"Run a shell command.",
		},
		{
			"hard cap fallback",
			"" +
				"This is one giant runaway sentence with no breaks at all that just goes on " +
				"and on past two hundred characters so the boundary search finds neither a " +
				"paragraph nor a newline before the cap, forcing the truncate path to fire.",
			"This is one giant runaway sentence with no breaks at all that just goes on and on past two hundred characters so the boundary search finds neither a paragraph nor a newline before the cap, forcing the…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shortToolDesc(tc.in)
			if got != tc.want {
				t.Errorf("shortToolDesc(%q) =\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}

func (*fakeTool) IsEnabled() bool { return true }

func TestDispatchHiddenToolFailsClosedBeforeExecution(t *testing.T) {
	started := &atomic.Int64{}
	reg := tools.NewRegistry()
	reg.Register(hiddenDispatchTool{&fakeTool{
		name: "InternalCredentialInjector", conc: tools.ConcurrencySafe, starts: started,
	}})
	loop := &Loop{Registry: reg}
	out := make(chan Event, 8)
	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "hidden", ToolName: "InternalCredentialInjector",
	}}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if started.Load() != 0 {
		t.Fatal("hidden tool executed after a guessed model call")
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, "unknown tool") {
		t.Fatalf("hidden tool should be indistinguishable from an unknown tool: %+v", results)
	}
}

func TestDispatchDeferredToolRequiresDiscoveryOnlyWhileSchemaIsLazy(t *testing.T) {
	t.Setenv("ENABLE_TOOL_SEARCH", "true")
	started := &atomic.Int64{}
	reg := tools.NewRegistry()
	reg.Register(deferredDispatchTool{&fakeTool{
		name: "RemoteDocumentSearch", conc: tools.ConcurrencySafe, starts: started,
	}})
	loop := &Loop{Registry: reg}
	out := make(chan Event, 16)
	use := llm.ContentBlock{Type: "tool_use", ToolUseID: "deferred", ToolName: "RemoteDocumentSearch"}

	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{use}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch before discovery: %v", err)
	}
	if started.Load() != 0 {
		t.Fatal("undiscovered deferred tool executed while its schema was lazy")
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, "ToolSearch") {
		t.Fatalf("undiscovered deferred call should direct the model to ToolSearch: %+v", results)
	}

	loop.markDeferredDiscovered("RemoteDocumentSearch")
	use.ToolUseID = "discovered"
	results, err = loop.executeBatch(context.Background(), []llm.ContentBlock{use}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch after discovery: %v", err)
	}
	if started.Load() != 1 || len(results) != 1 || results[0].IsError {
		t.Fatalf("discovered deferred tool should execute: starts=%d results=%+v", started.Load(), results)
	}
}

// TestDispatch_ExclusiveSkippedWhenCtxCancelled covers the 2026-05-18
// M2 fix: when the parent ctx dies between the safe/queue phase and
// the exclusive phase, the dispatcher must NOT call the exclusive
// tool's Execute (wasted work + N redundant ctx.Canceled errors).
// Instead it synthesizes one cancellation tool_result so the batch
// stays API-valid and the model sees one clear "interrupted" signal.
func TestDispatch_ExclusiveSkippedWhenCtxCancelled(t *testing.T) {
	t.Parallel()
	executed := &atomic.Int64{}
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "ExclA", conc: tools.ConcurrencyExclusive,
		starts: executed, hold: 0,
	})
	reg.Register(&fakeTool{
		name: "ExclB", conc: tools.ConcurrencyExclusive,
		starts: executed, hold: 0,
	})

	loop := &Loop{Registry: reg}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "a", ToolName: "ExclA"},
		{Type: "tool_use", ToolUseID: "b", ToolName: "ExclB"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead-on-arrival

	out := make(chan Event, 32)
	results, err := loop.executeBatch(ctx, uses, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch err: %v", err)
	}
	if got := executed.Load(); got != 0 {
		t.Errorf("expected zero Execute calls on cancelled ctx; got %d", got)
	}
	if len(results) != 2 {
		t.Fatalf("expected one tool_result per tool_use; got %d", len(results))
	}
	for i, r := range results {
		if r.Type != "tool_result" {
			t.Errorf("result[%d] type=%q want tool_result", i, r.Type)
		}
		if !r.IsError {
			t.Errorf("result[%d] should be IsError=true (cancellation)", i)
		}
		if r.ToolUseID == "" {
			t.Errorf("result[%d] missing ToolUseID — orphans the parent tool_use", i)
		}
	}
}
