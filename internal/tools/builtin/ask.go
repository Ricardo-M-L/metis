package builtin

import (
	"context"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// AskUser asks the user a question and returns the answer.
type AskUser struct{ tools.BaseTool; gate *permission.Gate }

func (AskUser) Name() string { return "AskUser" }
func (AskUser) Description() string {
	return "Ask the user a question. Returns the user's answer. Blocked in non-interactive mode."
}
func (AskUser) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"question"},
		"properties": map[string]any{
			"question":       map[string]any{"type": "string"},
			"options":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"allow_freeform": map[string]any{"type": "boolean"},
		},
	}
}
func (AskUser) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencyExclusive }
func (a AskUser) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, _ := a.gate.Check(context.Background(), "AskUser", strFromAny(in["question"]))
	return mapDecision(d), "interactive"
}

func (AskUser) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	question, _ := in["question"].(string)
	return &tools.Result{Output: "interactive: " + question}, nil
}
