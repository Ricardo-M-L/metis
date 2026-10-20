package tool

import "context"

// OperationExecutor supplies a durable execution boundary for a tool call.
// invoke performs the actual call and must report recovered panics as errors.
// The executor may replay a confirmed result or refuse an uncertain operation.
// It is intentionally optional: ordinary interactive calls keep their existing
// execution lifecycle and third-party tools need no new methods.
type OperationExecutor interface {
	Execute(ctx context.Context, tool Tool, input map[string]any, invoke func(context.Context) (*Result, error)) (*Result, error)
}

type operationExecutorContextKey struct{}

// WithOperationExecutor scopes durable execution to this context and its
// descendants. Context values also survive context.WithoutCancel, so detached
// subagents remain fenced by the same persistent claim at each writing call.
func WithOperationExecutor(ctx context.Context, executor OperationExecutor) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if executor == nil {
		return ctx
	}
	return context.WithValue(ctx, operationExecutorContextKey{}, executor)
}

// OperationExecutorFromContext returns the optional durable boundary.
func OperationExecutorFromContext(ctx context.Context) OperationExecutor {
	if ctx == nil {
		return nil
	}
	executor, _ := ctx.Value(operationExecutorContextKey{}).(OperationExecutor)
	return executor
}
