package openai

import (
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
)

func init() {
	transport.Register("openai_chat", build)
	transport.Register("openai", build)
	transport.Register("openai_responses", buildResponses)
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
	p := New(opts.APIKey, opts.BaseURL, opts.Model, maxTokens, timeout, 0)
	if opts.ContextWindow > 0 {
		p.ContextWindow = opts.ContextWindow
	}
	return &transport.Result{Provider: p, Model: p.Model, MaxOutputTokens: maxTokens}, nil
}

func buildResponses(opts transport.BuildOpts) (*transport.Result, error) {
	timeout := time.Duration(opts.Timeout) * time.Second
	if opts.Timeout <= 0 {
		timeout = 120 * time.Second
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = transport.DefaultMaxOutputTokens
	}
	p := NewResponses(opts.APIKey, opts.BaseURL, opts.Model, maxTokens, timeout, 0)
	if opts.ContextWindow > 0 {
		p.ContextWindow = opts.ContextWindow
	}
	return &transport.Result{Provider: p, Model: p.Model, MaxOutputTokens: maxTokens}, nil
}
