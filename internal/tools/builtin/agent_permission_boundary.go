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

// validateAgentPermissionOverride prevents a child from manufacturing a more
// permissive posture than the runtime boundary its parent already owns. In
// particular, a default parent cannot safely request a bypass child: the
// concrete tools and shared subprocess sandbox still belong to the parent
// runtime. Users who want unattended multi-agent execution must enter bypass
// at the top level first; children then inherit it without another prompt.
func validateAgentPermissionOverride(parent permission.Mode, in map[string]any) error {
	requested, hasRequested, err := requestedAgentPermissionMode(in)
	if err != nil || !hasRequested {
		return err
	}
	if agentPermissionModeEscalates(parent, requested) {
		return fmt.Errorf(
			"Agent permission_mode=%q cannot be more permissive than parent mode %q; change the top-level permission mode first",
			requested, permission.CanonicalMode(string(parent)),
		)
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

func (t agentPermissionBoundTool) ToolExposure() tools.ToolExposure {
	return tools.EffectiveExposure(t.inner)
}

func (t agentPermissionBoundTool) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	outerDecision := tools.PermissionAllow
	outerSource := ""
	if t.gate != nil {
		decision, source := t.gate.Check(ctx, t.Name(), marshalAgentToolInput(in))
		outerDecision = mapDecision(decision)
		if outerDecision != tools.PermissionAllow {
			outerSource = childPermissionReason(source)
		}
		if outerDecision == tools.PermissionDeny {
			return outerDecision, outerSource
		}
	}

	// ASK is not terminal here. Path-aware built-ins prepare their one-shot
	// invocation binding inside CanUse; skipping the inner call means the user can
	// approve the outer ASK only for Execute to fail with "permission binding
	// missing". Run both policy layers, let either DENY win, and collapse one or
	// two ASK decisions into the dispatcher's single approval prompt.
	innerDecision, innerSource := t.inner.CanUse(ctx, in)
	if innerDecision == tools.PermissionDeny {
		return innerDecision, innerPermissionReason(innerSource)
	}
	if outerDecision == tools.PermissionAsk || innerDecision == tools.PermissionAsk {
		return tools.PermissionAsk, combinePermissionReasons(outerSource, innerSource)
	}
	return tools.PermissionAllow, innerSource
}

func childPermissionReason(source string) string {
	if strings.TrimSpace(source) == "" {
		return "child permission gate"
	}
	return "child permission gate: " + source
}

func innerPermissionReason(source string) string {
	if strings.TrimSpace(source) == "" {
		return "tool permission gate"
	}
	return source
}

func combinePermissionReasons(outer, inner string) string {
	outer = strings.TrimSpace(outer)
	inner = strings.TrimSpace(inner)
	switch {
	case outer == "":
		return inner
	case inner == "":
		return outer
	case outer == inner:
		return outer
	default:
		return outer + "; tool: " + inner
	}
}

func (t agentPermissionBoundTool) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	return t.inner.Execute(ctx, in)
}

// PrepareAuthorizedInvocation delegates the structural, one-shot binding
// needed by path-aware built-ins. A fork has already applied its own immutable
// child gate before calling this method, so this must not re-enter either the
// child or parent permission policy.
func (t agentPermissionBoundTool) PrepareAuthorizedInvocation(ctx context.Context, in map[string]any) error {
	preparer, ok := t.inner.(tools.InvocationPreparer)
	if !ok {
		return nil
	}
	return preparer.PrepareAuthorizedInvocation(ctx, in)
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

func (t agentPermissionBoundTool) CanAutoAllowInBypass(in map[string]any) bool {
	return tools.CanAutoAllowInBypass(t.inner, in)
}

func (t agentPermissionBoundTool) InterruptBehavior() tools.InterruptBehavior {
	return tools.GetInterruptBehavior(t.inner)
}

func (t agentPermissionBoundTool) MaxResultSizeChars() int {
	return tools.MaxResultSizeChars(t.inner)
}

func (t agentPermissionBoundTool) TimeoutMs() int { return tools.TimeoutMs(t.inner) }

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
