package azure

import (
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
)

func init() {
	transport.Register("azure_openai", build)
	transport.Register("azure", build)
}

// build reads Azure-specific extras from opts.Extra:
//
//	api_version  : the Azure API version query string (default 2024-08-01-preview)
//	auth_mode    : "" / "api-key" → api-key header (default); "aad"/"bearer" → Authorization
//
// opts.Model is the deployment name (Azure routes by deployment, not model id).
func build(opts transport.BuildOpts) (*transport.Result, error) {
	timeout := time.Duration(opts.Timeout) * time.Second
	if opts.Timeout <= 0 {
		timeout = 120 * time.Second
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	apiVersion := ""
	authMode := ""
	if opts.Extra != nil {
		apiVersion = opts.Extra["api_version"]
		authMode = opts.Extra["auth_mode"]
	}
	p := NewAzure(opts.APIKey, opts.BaseURL, opts.Model, apiVersion, opts.Model, maxTokens, timeout)
	p.AuthMode = authMode
	if opts.ContextWindow > 0 {
		p.ContextWindow = opts.ContextWindow
	}
	return &transport.Result{Provider: p, Model: opts.Model}, nil
}
