package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

func requestedAgentPermissionMode(in map[string]any) (permission.Mode, bool, error) {
	raw, present := in["permission_mode"]
	if !present || raw == nil {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", false, fmt.Errorf("permission_mode must be a string")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false, nil
	}
	mode, ok := permission.ParseMode(value)
	if !ok {
		return "", false, fmt.Errorf("unknown permission_mode %q (want default|acceptEdits|plan|dontAsk|bypassPermissions)", value)
	}
	return mode, true, nil
}

// agentPermissionModeEscalates reports whether an explicit child override is
// more permissive than the parent's current posture. The modes are not a
// simple numeric ladder: dontAsk is stricter than default because it turns
// would-be prompts into denials, while acceptEdits is broader only for edits.
// Keep the matrix explicit so a future mode addition fails closed instead of
// accidentally inheriting an arbitrary rank.
func agentPermissionModeEscalates(parent, requested permission.Mode) bool {
	parent = permission.CanonicalMode(string(parent))
	requested = permission.CanonicalMode(string(requested))
	if parent == requested || requested == permission.ModePlan {
		return false
	}
	switch parent {
	case permission.ModePlan:
		return true
	case permission.ModeDontAsk:
		return requested == permission.ModeDefault ||
			requested == permission.ModeAcceptEdits ||
			requested == permission.ModeBypassPermissions
	case permission.ModeDefault:
		return requested == permission.ModeAcceptEdits ||
			requested == permission.ModeBypassPermissions
	case permission.ModeAcceptEdits:
		return requested == permission.ModeBypassPermissions
	case permission.ModeBypassPermissions:
		return false
	default:
		return true
	}
}

func validatePlanAgentInput(parentWasPlan bool, in map[string]any) error {
	requested, hasRequested, err := requestedAgentPermissionMode(in)
	if err != nil {
		return err
	}
	if !parentWasPlan {
		return nil
	}
	if hasRequested && requested != permission.ModePlan {
		return fmt.Errorf("plan mode cannot start Agent with permission_mode=%q; omit permission_mode or use plan", requested)
	}
	if isolation, _ := in["isolation"].(string); strings.TrimSpace(isolation) == "worktree" {
		return fmt.Errorf("plan mode cannot start Agent with isolation=\"worktree\" because creating a git worktree changes repository metadata")
	}
	return nil
}

// planChildBlockedTool prevents a read-only planning child from changing its
// own permission posture, invoking trusted Skill shell expansions, or
// creating another agent whose permission context would be harder to audit.
// The parent remains responsible for approving the plan and starting a fresh
// implementation turn.
func planChildBlockedTool(name string) bool {
	switch name {
	case "Agent", "Fork", "EnterPlanMode", "ExitPlanMode", "Skill":
		return true
	default:
		return false
	}
}

// agentChildRegistry always creates a distinct registry and wraps every
// retained tool with the child's cloned gate. The concrete built-ins in the
// parent registry capture the parent's gate, so copying their interface values
// alone does not establish a child permission boundary: a background child
// would otherwise follow later parent mode changes. The wrapper checks the
// immutable child gate first, then preserves the original tool's own CanUse
// check as a second, potentially stricter policy layer.
func agentChildRegistry(src *tools.Registry, childGate *permission.Gate, planLocked bool) *tools.Registry {
	dst := tools.NewRegistry()
	if src == nil {
		return dst
	}
	for _, inner := range src.All() {
		if planLocked && planChildBlockedTool(inner.Name()) {
			continue
		}
		dst.Register(agentPermissionBoundTool{inner: inner, gate: childGate})
	}
	return dst
}

// agentPermissionBoundTool preserves the public Tool surface and optional
// capabilities while adding the child-gate check in front of the original
// implementation.
type agentPermissionBoundTool struct {
	inner tools.Tool
	gate  *permission.Gate
}

func (t agentPermissionBoundTool) Name() string { return t.inner.Name() }

func (t agentPermissionBoundTool) Description() string { return t.inner.Description() }

func (t agentPermissionBoundTool) InputSchema() map[string]any { return t.inner.InputSchema() }

func (t agentPermissionBoundTool) Concurrency(in map[string]any) tools.Concurrency {
	return t.inner.Concurrency(in)
}

func (t agentPermissionBoundTool) IsEnabled() bool { return t.inner.IsEnabled() }

func (t agentPermissionBoundTool) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	if t.gate != nil {
		decision, source := t.gate.Check(ctx, t.Name(), marshalAgentToolInput(in))
		if decision != permission.DecisionAllow {
			if source == "" {
				source = "child permission gate"
			} else {
				source = "child permission gate: " + source
			}
			return mapDecision(decision), source
		}
	}
	return t.inner.CanUse(ctx, in)
}

func (t agentPermissionBoundTool) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	return t.inner.Execute(ctx, in)
}

func marshalAgentToolInput(in map[string]any) string {
	b, err := json.Marshal(in)
	if err != nil {
		return fmt.Sprint(in)
	}
	return string(b)
}

// Optional capabilities are delegated so wrapping does not silently change
// scheduling, rendering, safety highlighting, aliases, or spill behavior.
func (t agentPermissionBoundTool) IsReadOnly(in map[string]any) bool {
	return tools.IsReadOnly(t.inner, in)
}

func (t agentPermissionBoundTool) IsDestructive(in map[string]any) bool {
	return tools.IsDestructive(t.inner, in)
}

func (t agentPermissionBoundTool) RequiresUserInteraction() bool {
	return tools.RequiresUserInteraction(t.inner)
}

func (t agentPermissionBoundTool) IsBypassImmune(in map[string]any) (bool, string) {
	return tools.IsBypassImmune(t.inner, in)
}

func (t agentPermissionBoundTool) InterruptBehavior() tools.InterruptBehavior {
	return tools.GetInterruptBehavior(t.inner)
}

func (t agentPermissionBoundTool) MaxResultSizeChars() int {
	return tools.MaxResultSizeChars(t.inner)
}

func (t agentPermissionBoundTool) Aliases() []string { return pubtool.Aliases(t.inner) }

func (t agentPermissionBoundTool) SearchHint() string { return pubtool.SearchHint(t.inner) }

func (t agentPermissionBoundTool) ShortDescription() string {
	if short, ok := t.inner.(tools.ShortDescriptor); ok {
		return short.ShortDescription()
	}
	full := t.inner.Description()
	if idx := strings.Index(full, "\n"); idx >= 0 {
		full = full[:idx]
	}
	if len(full) > 200 {
		full = full[:200]
	}
	return strings.TrimSpace(full)
}
