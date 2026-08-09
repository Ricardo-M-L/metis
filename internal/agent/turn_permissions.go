package agent

import (
	"context"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// turnPermissionRulesKey scopes temporary custom-command approvals to one
// Loop.Run. Keeping the rules in context avoids mutating the session-wide gate
// before the asynchronous TUI goroutine starts and makes the lifetime explicit.
type turnPermissionRulesKey struct{}

// WithTurnPermissionRules returns a child context carrying permission rules
// that apply only to the next Loop.Run invoked with that context. The slice is
// copied so callers cannot mutate live policy after dispatch.
func WithTurnPermissionRules(ctx context.Context, rules []permission.Rule) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(rules) == 0 {
		return ctx
	}
	copyRules := append([]permission.Rule(nil), rules...)
	return context.WithValue(ctx, turnPermissionRulesKey{}, copyRules)
}

func turnPermissionRulesFromContext(ctx context.Context) []permission.Rule {
	if ctx == nil {
		return nil
	}
	rules, _ := ctx.Value(turnPermissionRulesKey{}).([]permission.Rule)
	return append([]permission.Rule(nil), rules...)
}
