package tui

import (
	"context"
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// loggedOutProvider replaces a provider that cached an API key in memory
// after /logout removes the durable credential. Keeping a non-nil provider
// gives an active agent loop a deterministic, actionable failure instead of
// either continuing with the revoked key or panicking on its next request.
type loggedOutProvider struct {
	name          string
	model         string
	contextWindow int
}

func (p *loggedOutProvider) Name() string { return p.name }

func (p *loggedOutProvider) ModelID() string { return p.model }

func (p *loggedOutProvider) MaxContextTokens() int { return p.contextWindow }

func (p *loggedOutProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, p.authError()
}

func (p *loggedOutProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, p.authError()
}

func (p *loggedOutProvider) authError() error {
	return fmt.Errorf("provider %q is logged out; run `metis login %s`, then switch back to the provider", p.name, p.name)
}
