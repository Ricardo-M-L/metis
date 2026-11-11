package anthropic

func (a *Anthropic) DefaultOutputBudget() int { return a.MaxTokens }
