package llm

func (p *executionBoundProvider) DefaultOutputBudget() int {
	return ProviderDefaultOutputBudget(p.Provider)
}
