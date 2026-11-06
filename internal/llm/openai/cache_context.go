package openai

// DynamicContextCheckpoints reports whether this exact transport can replay
// append-only, privileged dynamic snapshots without flattening them ahead of
// history. Provider-managed instructions are replaced independently instead.
func (r *Responses) DynamicContextCheckpoints() bool {
	return r.effectiveStateMode() == ResponsesStateLocal
}

func (o *OpenAI) DynamicContextCheckpoints() bool { return o.TailSystemMessages }
