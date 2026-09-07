package memory

// DistillationProviderError identifies failure of optional fact extraction,
// before any extracted facts are written. Callers may treat this as a warning
// after the user's task succeeds, but must not downgrade unrelated persistence
// errors returned by DistillTurnWithMetadata.
type DistillationProviderError struct {
	Err error
}

func (e *DistillationProviderError) Error() string {
	return "distill: provider error: " + e.Err.Error()
}

func (e *DistillationProviderError) Unwrap() error { return e.Err }
