package llm

import "errors"

// ErrContextOutputBudget rejects generation when a configured output request
// exceeds a validated model/route ceiling. Unlike unavailable discovery, this
// is a configuration error and must not be retried as a transport failure.
var ErrContextOutputBudget = errors.New("output budget exceeds advertised model limit")
