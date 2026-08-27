package bedrock

import (
	"os"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
)

func init() {
	transport.Register("bedrock_anthropic", build)
	transport.Register("bedrock", build)
}

// build reads Bedrock-specific extras from opts.Extra:
//
//	secret_key_env     : env var name holding AWS_SECRET_ACCESS_KEY
//	session_token_env  : env var name for STS-issued session tokens
//	region             : AWS region (also acceptable via opts.BaseURL)
//
// opts.APIKey carries the AWS_ACCESS_KEY_ID (resolved by metis's
// ResolveAPIKey via the profile's api_key_env). The secret key + token
// flow through env vars whose names are configured per-profile so the
// secret never lands in config.toml.
func build(opts transport.BuildOpts) (*transport.Result, error) {
	timeout := time.Duration(opts.Timeout) * time.Second
	if opts.Timeout <= 0 {
		timeout = 120 * time.Second
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = transport.DefaultMaxOutputTokens
	}
	secret, sessionTok, region := "", "", ""
	if opts.Extra != nil {
		if env := opts.Extra["secret_key_env"]; env != "" {
			secret = os.Getenv(env)
		}
		if env := opts.Extra["session_token_env"]; env != "" {
			sessionTok = os.Getenv(env)
		}
		region = opts.Extra["region"]
	}
	if region == "" {
		region = opts.BaseURL
	}
	p, err := NewBedrock(opts.APIKey, secret, sessionTok, region, opts.Model, maxTokens, timeout)
	if err != nil {
		return nil, err
	}
	if opts.ContextWindow > 0 {
		p.ContextWindow = opts.ContextWindow
	}
	return &transport.Result{Provider: p, Model: opts.Model, MaxOutputTokens: maxTokens}, nil
}
