package llm

// DefaultOutputBudgetProvider exposes the adapter's effective per-response
// output reservation. A zero value means the transport leaves the limit to
// the server or does not expose it. It is not the model's maximum capacity.
type DefaultOutputBudgetProvider interface {
	DefaultOutputBudget() int
}

// ProviderDefaultOutputBudget is read-only. Request-specific caps must remain
// on the request, rather than mutating a client shared by parent and children.
func ProviderDefaultOutputBudget(provider Provider) int {
	if budget, ok := provider.(DefaultOutputBudgetProvider); ok {
		return max(0, budget.DefaultOutputBudget())
	}
	return 0
}
