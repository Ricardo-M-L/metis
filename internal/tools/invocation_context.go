package tools

import "context"

// invocationIDContextKey is private so only this package can manufacture the
// dispatch identity consumed by tools. The value is deliberately distinct
// from provider tool_use_id: providers may reuse their public identifier,
// while the dispatcher creates one fresh ID for every concrete call.
type invocationIDContextKey struct{}

// WithInvocationID binds permission preparation and execution to one concrete
// tool invocation. Dispatch must pass the same non-empty ID to CanUse and
// Execute; direct embedders that omit it take the tools' fail-closed recheck
// path instead of consuming another call's approval.
func WithInvocationID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, invocationIDContextKey{}, id)
}

// InvocationIDFromContext returns the dispatcher-owned call identity, or an
// empty string for legacy/direct execution.
func InvocationIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(invocationIDContextKey{}).(string)
	return id
}
