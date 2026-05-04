package vertex

import (
	"errors"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
)

func init() {
	transport.Register("vertex_anthropic", build)
	transport.Register("vertex", build)
}

// build reads Vertex-specific extras from opts.Extra:
//
//	service_account_file : path to a GCP service-account JSON (required)
//	project              : GCP project id (required)
//	region               : GCP region (default us-central1; can also flow via opts.BaseURL)
//
// opts.BaseURL is reused as a region fallback for terse user configs.
func build(opts transport.BuildOpts) (*transport.Result, error) {
	timeout := time.Duration(opts.Timeout) * time.Second
	if opts.Timeout <= 0 {
		timeout = 120 * time.Second
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	if opts.Extra == nil {
		return nil, errors.New("vertex_anthropic: extra fields required (service_account_file, project, region)")
	}
	saFile := opts.Extra["service_account_file"]
	project := opts.Extra["project"]
	region := opts.Extra["region"]
	if region == "" {
		region = opts.BaseURL // legacy: people may have put region in base_url
	}
	if saFile == "" {
		return nil, errors.New("vertex_anthropic: service_account_file is required")
	}
	if project == "" {
		return nil, errors.New("vertex_anthropic: project is required")
	}
	p, err := NewVertex(saFile, project, region, opts.Model, maxTokens, timeout)
	if err != nil {
		return nil, err
	}
	if opts.ContextWindow > 0 {
		p.ContextWindow = opts.ContextWindow
	}
	return &transport.Result{Provider: p, Model: opts.Model}, nil
}
