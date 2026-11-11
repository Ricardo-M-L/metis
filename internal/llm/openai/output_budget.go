package openai

func (o *OpenAI) DefaultOutputBudget() int { return o.MaxTokens }

func (r *Responses) DefaultOutputBudget() int {
	// The subscription protocol intentionally omits max_output_tokens.
	if r.Name() == "openai-codex" {
		return 0
	}
	return r.MaxTokens
}
