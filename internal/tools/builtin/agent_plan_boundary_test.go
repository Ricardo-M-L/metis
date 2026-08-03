package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func planAgentForBoundaryTest(gate *permission.Gate, reg *tools.Registry) Agent {
	tool := NewAgent(gate, helloProvider(), reg, "model", "system")
	// Wire the read-only hook to actually consult Agent.IsReadOnly so
	// the test exercises the production code path (capabilities.go).
	// The previous hook hardcoded `name == "Agent"` which short-
	// circuited IsReadOnly and hid the 2026-07-27 plan-mode behavior
	// change from these tests.
	gate.SetReadOnlyHook(func(name, stringInput string) bool {
		if name == "Agent" {
			// The hook receives a stringified input; Agent.IsReadOnly
			// only inspects structured fields (isolation, permission_mode),
			// both of which are absent here, so an empty map matches
			// production behavior for these tests.
			return tool.IsReadOnly(map[string]any{})
		}
		return name == "Read"
	})
	return tool
}

func TestAgentPlanBoundaryRejectsPermissionEscalation(t *testing.T) {
	for _, mode := range []string{"default", "acceptEdits", "dontAsk", "bypassPermissions"} {
		t.Run(mode, func(t *testing.T) {
			gate := permission.New(permission.ModePlan)
			tool := planAgentForBoundaryTest(gate, tools.NewRegistry())
			in := map[string]any{"prompt": "inspect", "permission_mode": mode}
			got, reason := tool.CanUse(context.Background(), in)
			if got != tools.PermissionDeny || !strings.Contains(reason, "plan mode cannot start Agent") {
				t.Fatalf("CanUse(%s) = (%v, %q), want plan-boundary deny", mode, got, reason)
			}
			res, err := tool.Execute(context.Background(), in)
			if err != nil {
				t.Fatalf("Execute(%s): %v", mode, err)
			}
			if !res.IsError || !strings.Contains(res.Output, "plan mode cannot start Agent") {
				t.Fatalf("Execute(%s) = %+v, want plan-boundary error", mode, res)
			}
		})
	}
}

func TestAgentPlanBoundaryRejectsWorktreeBeforeSpawn(t *testing.T) {
	gate := permission.New(permission.ModePlan)
	tool := planAgentForBoundaryTest(gate, tools.NewRegistry())
	in := map[string]any{"prompt": "inspect", "isolation": "worktree"}
	if got, reason := tool.CanUse(context.Background(), in); got != tools.PermissionDeny || !strings.Contains(reason, "git worktree") {
		t.Fatalf("CanUse = (%v, %q), want worktree side-effect deny", got, reason)
	}
	res, err := tool.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Output, "git worktree") {
		t.Fatalf("Execute = %+v, want failure before resolveIsolation", res)
	}
}

func TestAgentPlanBoundaryDeniesBackgroundChild(t *testing.T) {
	// 2026-07-27: previously this test asserted Allow — the read-only
	// hook flagged Agent as side-effect-free when no permission_mode
	// was requested, so plan-mode parents could spawn children at
	// will. The fix in capabilities.go Agent.IsReadOnly now returns
	// false whenever the parent is in ModePlan, so even background
	// spawns are denied at the parent boundary. The downstream
	// validatePlanAgentInput still refuses non-plan overrides; this
	// test locks in the stricter parent-side deny.
	gate := permission.New(permission.ModePlan)
	tool := planAgentForBoundaryTest(gate, tools.NewRegistry())
	got, reason := tool.CanUse(context.Background(), map[string]any{
		"prompt":            "inspect in parallel",
		"run_in_background": true,
	})
	if got != tools.PermissionDeny {
		t.Fatalf("plan-mode parent Agent CanUse = (%v, %q), want deny (2026-07-27 contract)", got, reason)
	}
}

func TestAgentIsReadOnlyPermissionEscalationMatrix(t *testing.T) {
	// True entries are the only explicit child overrides that broaden the
	// parent's posture. All other pairs are equal or restrictive and keep
	// Claude Code's no-extra-spawn-approval behavior.
	elevating := map[permission.Mode]map[permission.Mode]bool{
		permission.ModePlan: {
			permission.ModeDefault:           true,
			permission.ModeAcceptEdits:       true,
			permission.ModeDontAsk:           true,
			permission.ModeBypassPermissions: true,
		},
		permission.ModeDontAsk: {
			permission.ModeDefault:           true,
			permission.ModeAcceptEdits:       true,
			permission.ModeBypassPermissions: true,
		},
		permission.ModeDefault: {
			permission.ModeAcceptEdits:       true,
			permission.ModeBypassPermissions: true,
		},
		permission.ModeAcceptEdits: {
			permission.ModeBypassPermissions: true,
		},
		permission.ModeBypassPermissions: {},
	}

	for _, parent := range permission.Modes {
		t.Run("parent_"+string(parent), func(t *testing.T) {
			tool := NewAgent(permission.New(parent), helloProvider(), tools.NewRegistry(), "m", "s")
			noOverrideReadOnly := tool.IsReadOnly(map[string]any{"prompt": "delegate"})
			// 2026-07-27: when the parent is in ModePlan, Agent is no
			// longer advertised as read-only even without an explicit
			// permission_mode override. Reasoning: plan mode's
			// user-visible contract is "stop and propose a plan";
			// allowing Agent through the read-only path let the model
			// spin up arbitrarily many read-only children to keep
			// exploring, which made plan mode indistinguishable from
			// a normal turn. See capabilities.go Agent.IsReadOnly.
			if parent == permission.ModePlan {
				if noOverrideReadOnly {
					t.Error("plan-mode parent must NOT advertise Agent as read-only (2026-07-27 plan-mode contract)")
				}
			} else if !noOverrideReadOnly {
				t.Error("no-override Agent should remain read-only at the parent boundary")
			}
			if tool.IsReadOnly(map[string]any{"prompt": "delegate", "isolation": "worktree"}) {
				t.Error("worktree Agent must never be advertised as read-only")
			}
			for _, requested := range permission.Modes {
				wantEscalation := elevating[parent][requested]
				if got := agentPermissionModeEscalates(parent, requested); got != wantEscalation {
					t.Errorf("agentPermissionModeEscalates(%s, %s) = %v, want %v", parent, requested, got, wantEscalation)
				}
				gotReadOnly := tool.IsReadOnly(map[string]any{
					"prompt":          "delegate",
					"permission_mode": string(requested),
				})
				// In plan mode, IsReadOnly is unconditionally false
				// regardless of the requested override — the
				// escalation matrix still governs the downstream
				// validatePlanAgentInput refusal, but the read-only
				// fast-path is closed off entirely.
				if parent == permission.ModePlan {
					if gotReadOnly {
						t.Errorf("IsReadOnly parent=%s requested=%s = true, want false (plan-mode parent always blocks Agent via read-only path)", parent, requested)
					}
					continue
				}
				if gotReadOnly == wantEscalation {
					t.Errorf("IsReadOnly parent=%s requested=%s = %v, want %v", parent, requested, gotReadOnly, !wantEscalation)
				}
			}
		})
	}
}

type parentGateBoundBoundaryTool struct {
	tools.BaseTool
	name string
	gate *permission.Gate
}

func (t parentGateBoundBoundaryTool) Name() string        { return t.name }
func (t parentGateBoundBoundaryTool) Description() string { return "boundary test tool" }
func (t parentGateBoundBoundaryTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t parentGateBoundBoundaryTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (t parentGateBoundBoundaryTool) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	decision, source := t.gate.Check(ctx, t.name, marshalAgentToolInput(in))
	return mapDecision(decision), source
}
func (t parentGateBoundBoundaryTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "executed"}, nil
}

func TestAgentPlanChildRegistryFreezesGateAndRemovesEscapeTools(t *testing.T) {
	parentGate := permission.New(permission.ModePlan)
	parentGate.SetReadOnlyHook(func(name, _ string) bool { return name == "Read" })
	parent := tools.NewRegistry()
	for _, name := range []string{"Read", "Write", "Agent", "Fork", "EnterPlanMode", "ExitPlanMode", "Skill"} {
		parent.Register(parentGateBoundBoundaryTool{name: name, gate: parentGate})
	}

	childGate := parentGate.Clone()
	child := agentChildRegistry(parent, childGate, true)
	for _, blocked := range []string{"Agent", "Fork", "EnterPlanMode", "ExitPlanMode", "Skill"} {
		if _, ok := child.Get(blocked); ok {
			t.Errorf("Plan child registry still exposes %s", blocked)
		}
	}

	// Simulate approving the parent plan while an already-running background
	// child continues. Its concrete tools still point at parentGate, now bypass,
	// but the outer cloned child gate must remain Plan and refuse Write.
	parentGate.SetMode(permission.ModeBypassPermissions)
	write, ok := child.Get("Write")
	if !ok {
		t.Fatal("Write should remain visible so denied calls produce a useful policy result")
	}
	if got, reason := write.CanUse(context.Background(), map[string]any{"path": "/tmp/x"}); got != tools.PermissionDeny || !strings.Contains(reason, "child permission gate") {
		t.Fatalf("Write after parent mode drift = (%v, %q), want child Plan deny", got, reason)
	}

	read, ok := child.Get("Read")
	if !ok {
		t.Fatal("Plan child registry lost Read")
	}
	if got, reason := read.CanUse(context.Background(), map[string]any{"path": "/tmp/x"}); got != tools.PermissionAllow {
		t.Fatalf("Read after parent mode drift = (%v, %q), want allow", got, reason)
	}
}
