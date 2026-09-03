package anthropic

// Self-register the anthropic_messages transport with the package-level
// registry. runtime/provider.go does a blank import of this package to
// trigger the init() side effect — no central switch to update when
// new providers land.

import (
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
)

func init() {
	transport.Register("anthropic_messages", build)
	transport.Register("anthropic", build) // alias for terse user configs
}

func build(opts transport.BuildOpts) (*transport.Result, error) {
	timeout := time.Duration(opts.Timeout) * time.Second
	if opts.Timeout <= 0 {
		timeout = 120 * time.Second
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = transport.DefaultMaxOutputTokens
	}
	beta := ""
	if opts.Extra != nil {
		beta = opts.Extra["beta"]
	}
	p := New(opts.APIKey, opts.BaseURL, opts.Model, maxTokens, timeout, beta)
	p.CatalogProvider = opts.CatalogProvider
	if opts.ContextWindow > 0 {
		p.ContextWindow = opts.ContextWindow
	}
	return &transport.Result{Provider: p, Model: p.Model, MaxOutputTokens: maxTokens}, nil
}
