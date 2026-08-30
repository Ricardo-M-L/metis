package agent

import "context"

// ToolAdmissionPolicy is an unattended, in-process authorization boundary
// evaluated for every non-read-only tool after PreToolUse hooks have finalized
// its arguments and before either CanUse or Execute. input is a defensive copy
// of the exact execution arguments and is policy-only: implementations must
// never render, log, trace, audit, or persist it.
//
// Interactive sessions leave this unset. Cron installs it on the turn context
// so the boundary also follows that context into ordinary child-agent loops
// without becoming mutable process-global Loop state.
type ToolAdmissionPolicy func(tool string, input map[string]any) (allow bool, reason string)

type toolAdmissionPolicyKey struct{}

// WithToolAdmissionPolicy scopes policy to ctx and its descendants. A nil
// policy is a no-op. Context scope is deliberate: one unattended run cannot
// leak its job allow-list into a later interactive turn on the same Loop.
func WithToolAdmissionPolicy(ctx context.Context, policy ToolAdmissionPolicy) context.Context {
	if policy == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, toolAdmissionPolicyKey{}, policy)
}

func toolAdmissionPolicyFromContext(ctx context.Context) ToolAdmissionPolicy {
	if ctx == nil {
		return nil
	}
	policy, _ := ctx.Value(toolAdmissionPolicyKey{}).(ToolAdmissionPolicy)
	return policy
}
