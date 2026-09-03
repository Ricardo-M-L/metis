package openai

import (
	"strings"
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
	p.CatalogProvider = opts.CatalogProvider
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
	if opts.Extra != nil {
		if err := p.ConfigureStateMode(opts.Extra["responses_state_mode"]); err != nil {
			return nil, err
		}
		if err := p.ConfigureCapabilityProfile(opts.Extra["responses_profile"]); err != nil {
			return nil, err
		}
		p.PromptCacheKey = strings.TrimSpace(opts.Extra["prompt_cache_key"])
		for _, tool := range strings.Split(opts.Extra["hosted_tools"], ",") {
			if tool = strings.TrimSpace(tool); tool != "" {
				p.HostedTools = append(p.HostedTools, tool)
			}
		}
	}
	if opts.ContextWindow > 0 {
		p.ContextWindow = opts.ContextWindow
	}
	return &transport.Result{Provider: p, Model: p.Model, MaxOutputTokens: maxTokens}, nil
}
