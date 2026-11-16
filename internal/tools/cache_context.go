package tools

import "context"

type cacheSummaryContextKey struct{}

// WithCacheSummary binds detached diagnostics to the loop invoking a tool.
func WithCacheSummary(ctx context.Context, summary string) context.Context {
	return context.WithValue(ctx, cacheSummaryContextKey{}, summary)
}

func CacheSummaryFromContext(ctx context.Context) (string, bool) {
	summary, ok := ctx.Value(cacheSummaryContextKey{}).(string)
	return summary, ok
}
