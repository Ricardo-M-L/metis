package tools

import "context"

// ResultProjectionObserver is an optional, internal tool capability. A tool may
// grant authority based on content the model has actually received only after
// the agent has admitted and durably recorded its projected result.
type ResultProjectionObserver interface {
	ObserveResultProjection(context.Context, map[string]any, bool)
}

type resultProjectionKey struct{}

// WithResultProjection tells a tool that its result still has to pass the
// agent's batch budget. Direct tool callers retain their synchronous contract.
func WithResultProjection(ctx context.Context) context.Context {
	return context.WithValue(ctx, resultProjectionKey{}, true)
}

func ResultProjectionPending(ctx context.Context) bool {
	pending, _ := ctx.Value(resultProjectionKey{}).(bool)
	return pending
}
