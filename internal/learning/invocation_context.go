package learning

import "context"

// Invocation is supplied by the host tool after resolving its actual content.
// The JSON tool protocol cannot install this observer or supply an activation.
type Invocation struct {
	Kind            string `json:"kind"`
	ArtifactRoot    string `json:"artifact_root"`
	Name            string `json:"name"`
	VersionSHA256   string `json:"version_sha256"`
	ActivationEpoch uint64 `json:"activation_epoch"`
}
type InvocationFinish func(context.Context, []byte) error
type InvocationObserver func(context.Context, Invocation) (InvocationFinish, error)
type invocationObserverKey struct{}

func WithInvocationObserver(ctx context.Context, observer InvocationObserver) context.Context {
	return context.WithValue(ctx, invocationObserverKey{}, observer)
}

func BeginInvocation(ctx context.Context, invocation Invocation) (InvocationFinish, error) {
	if observer, _ := ctx.Value(invocationObserverKey{}).(InvocationObserver); observer != nil {
		return observer(ctx, invocation)
	}
	return func(context.Context, []byte) error { return nil }, nil
}
