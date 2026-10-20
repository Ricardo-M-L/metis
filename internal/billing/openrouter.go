package billing

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

type OpenRouterConfig struct {
	APIBaseURL, InferenceEndpoint, LedgerProvider, Token string
}

type OpenRouter struct {
	base, provider, endpoint, token string
	client                          *http.Client
}

func NewOpenRouter(config OpenRouterConfig) (*OpenRouter, error) {
	if config.APIBaseURL == "" {
		config.APIBaseURL = "https://openrouter.ai/api/v1"
	}
	base := strings.TrimRight(config.APIBaseURL, "/")
	u, err := url.Parse(base)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, failure("invalid_openrouter_endpoint")
	}
	// An arbitrary compatible gateway does not establish OpenRouter authority.
	// Loopback HTTP is supported for local contract tests only.
	ip := net.ParseIP(u.Hostname())
	if base != "https://openrouter.ai/api/v1" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, failure("unsupported_openrouter_endpoint")
	}
	if config.LedgerProvider == "" || strings.TrimSpace(config.Token) == "" || strings.ContainsAny(config.Token, "\r\n") {
		return nil, failure("invalid_openrouter_credentials")
	}
	endpoint, err := EndpointSHA256(config.InferenceEndpoint)
	if err != nil {
		return nil, err
	}
	return &OpenRouter{base: base, provider: config.LedgerProvider, endpoint: endpoint, token: config.Token, client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

var generationID = regexp.MustCompile(`^gen-[0-9A-Za-z-]{1,124}$`)

// Lookup uses OpenRouter's documented generation record, not the optional
// streaming cost estimate. Official reference:
// https://openrouter.ai/docs/api/api-reference/generations/get-request-&-usage-metadata-for-a-generation
func (s *OpenRouter) Lookup(ctx context.Context, request Request) (Observation, error) {
	if err := bind(request, s.provider, s.endpoint); err != nil {
		return Observation{}, err
	}
	id := request.ResponseID
	if id == "" && generationID.MatchString(request.ProviderRequestID) {
		id = request.ProviderRequestID
	}
	if !generationID.MatchString(id) {
		return Observation{}, ErrPending
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/generation?id="+url.QueryEscape(id), nil)
	if err != nil {
		return Observation{}, failure("request_failed")
	}
	httpRequest.Header.Set("Authorization", "Bearer "+s.token)
	httpRequest.Header.Set("Cache-Control", "no-cache")
	response, err := s.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return Observation{}, ctx.Err()
		}
		return Observation{}, failure("lookup_failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusAccepted {
		return Observation{}, ErrPending
	}
	if response.StatusCode != http.StatusOK {
		return Observation{}, failure("lookup_http_status")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, EvidenceLimit+1))
	if err != nil || len(raw) > EvidenceLimit {
		return Observation{}, failure("evidence_size_limit")
	}
	if err := uniqueJSON(raw); err != nil {
		return Observation{}, err
	}
	var body struct {
		Data *struct {
			ID           string   `json:"id"`
			RequestID    string   `json:"request_id"`
			Model        string   `json:"model"`
			FinishReason string   `json:"finish_reason"`
			Cancelled    bool     `json:"cancelled"`
			IsBYOK       *bool    `json:"is_byok"`
			Prompt       *int     `json:"native_tokens_prompt"`
			Completion   *int     `json:"native_tokens_completion"`
			Cached       *int     `json:"native_tokens_cached"`
			Cost         *float64 `json:"total_cost"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return Observation{}, failure("invalid_generation_record")
	}
	if body.Data == nil {
		return Observation{}, ErrPending
	}
	d := body.Data
	if d.ID != id || d.Model != request.Model {
		return Observation{}, failure("generation_scope_mismatch")
	}
	if request.ProviderRequestID != "" && request.ProviderRequestID != id && d.RequestID != request.ProviderRequestID {
		return Observation{}, failure("generation_request_id_mismatch")
	}
	if d.Cost == nil || d.Prompt == nil || d.Completion == nil || d.IsBYOK == nil || (d.FinishReason == "" && !d.Cancelled) {
		return Observation{}, ErrPending
	}
	if math.IsNaN(*d.Cost) || math.IsInf(*d.Cost, 0) || *d.Cost < 0 {
		return Observation{}, failure("invalid_charge")
	}
	cached := 0
	if d.Cached != nil {
		cached = *d.Cached
	}
	if *d.Prompt < 0 || *d.Completion < 0 || cached < 0 || cached > *d.Prompt {
		return Observation{}, failure("invalid_token_counts")
	}
	request.ResponseID = id
	actual := d.Cost
	// With BYOK the provider invoices inference separately. OpenRouter's
	// record cannot establish that provider account's final total charge.
	if *d.IsBYOK {
		actual = nil
	}
	return observation(request, "openrouter/generation", projectcoord.TokenUsage{Input: *d.Prompt - cached, Output: *d.Completion, CacheRead: cached}, actual, raw)
}
