package azure

// Azure OpenAI Service. Same wire format as OpenAI but:
//
//   - URL: https://{resource}.openai.azure.com/openai/deployments/{deployment}/chat/completions?api-version={ver}
//   - Auth header: `api-key: <key>` (instead of `Authorization: Bearer <key>`)
//   - Model id in the request body is ignored — Azure routes by the
//     {deployment} segment of the URL.
//
// AAD (Microsoft Entra) bearer auth is NOT implemented yet; users on
// Entra-only endpoints should set up a service principal client_secret
// flow externally and pass the bearer token via APIKey + AuthMode="aad".
//
// Body translation (toOpenAI / fromOpenAIChoice) and SSE parsing
// (openAIStream) are shared with the OpenAI provider — Azure only
// differs at the transport layer.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

type (
	Request      = provider.Request
	Response     = provider.Response
	StreamReader = provider.StreamReader
	Message      = provider.Message
	ContentBlock = provider.ContentBlock
)

const (
	RoleSystem    = provider.RoleSystem
	RoleUser      = provider.RoleUser
	RoleAssistant = provider.RoleAssistant
	RoleTool      = provider.RoleTool
)

// Azure speaks Azure OpenAI's deployment-routed flavor of the OpenAI
// chat completions API.
type Azure struct {
	APIKey     string
	BaseURL    string // https://{resource}.openai.azure.com  (no trailing slash)
	Deployment string // Azure deployment name (not the model id; user-defined alias)
	APIVersion string // e.g. "2024-08-01-preview"
	Model      string // Echoed in the request body (Azure ignores it but the SDK requires it; usually = Deployment)
	MaxTokens  int
	httpClient *http.Client

	// AuthMode picks header shape. "" or "api-key" → `api-key: <key>` (default).
	// "aad" or "bearer" → `Authorization: Bearer <token>` for callers passing
	// an Entra-issued access token in APIKey.
	AuthMode string

	// ContextWindow override. Azure deployments inherit their model's
	// window but the deployment id doesn't tell us which model — caller
	// must set this when using compaction.
	ContextWindow int
}

// NewAzure builds an Azure OpenAI provider. resource = the Azure
// resource subdomain (e.g. "my-resource" → my-resource.openai.azure.com).
// deployment = the Azure deployment name. apiVersion is required by
// Azure; pass the latest preview unless your deployment is pinned.
func NewAzure(apiKey, resource, deployment, apiVersion, model string, maxTokens int, timeout time.Duration) *Azure {
	resource = strings.TrimSpace(resource)
	if resource == "" {
		resource = "" // surfaced as auth error in Complete
	}
	base := resource
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		base = "https://" + resource + ".openai.azure.com"
	}
	if apiVersion == "" {
		apiVersion = "2024-08-01-preview"
	}
	if model == "" {
		model = deployment // common default
	}
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Azure{
		APIKey:     apiKey,
		BaseURL:    base,
		Deployment: deployment,
		APIVersion: apiVersion,
		Model:      model,
		MaxTokens:  maxTokens,
		httpClient: transport.NewHTTPClient(timeout, "azure"),
	}
}

func (a *Azure) Name() string { return "azure" }

// MaxContextTokens returns the configured override or 0 (caller falls
// back to the model-prefix lookup, which is unlikely to match Azure
// deployment names — set context_window in config.toml for accuracy).
func (a *Azure) MaxContextTokens() int { return a.ContextWindow }

// chatURL renders the deployment-routed endpoint with the api-version
// query param Azure requires on every call.
func (a *Azure) chatURL() string {
	q := url.Values{}
	q.Set("api-version", a.APIVersion)
	return fmt.Sprintf("%s/openai/deployments/%s/chat/completions?%s",
		strings.TrimRight(a.BaseURL, "/"),
		url.PathEscape(a.Deployment),
		q.Encode())
}

func (a *Azure) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	mode := strings.ToLower(strings.TrimSpace(a.AuthMode))
	if mode == "aad" || mode == "bearer" {
		r.Header.Set("Authorization", "Bearer "+a.APIKey)
		return
	}
	r.Header.Set("api-key", a.APIKey)
}

func (a *Azure) preflight() error {
	if a.APIKey == "" {
		return errors.New("API key not configured. Set AZURE_API_KEY environment variable or configure in ~/.metis/config.toml")
	}
	if a.Deployment == "" {
		return errors.New("Azure provider: deployment is required (Azure routes by deployment name, not model id)")
	}
	if !strings.Contains(a.BaseURL, "openai.azure.com") && !strings.Contains(a.BaseURL, "cognitiveservices.azure.com") {
		// Be permissive — private endpoints / proxies may use other
		// hostnames. Just warn-via-comment that a typo here looks like
		// a network error, not a config error. No actual reject.
	}
	return nil
}

func (a *Azure) Complete(ctx context.Context, req Request) (*Response, error) {
	if err := a.preflight(); err != nil {
		return nil, err
	}
	body := openai.ToRequest(req, a.Model, a.MaxTokens)
	body.Stream = false

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	var or openai.Resp
	err = transport.RetryWithBackoff(ctx, 3, 0, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", a.chatURL(), bytes.NewReader(buf))
		if err != nil {
			return err
		}
		a.setHeaders(httpReq)
		resp, err := a.httpClient.Do(httpReq)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			httpErr := fmt.Errorf("azure %d: %s", resp.StatusCode, transport.Truncate(string(rb), 500))
			if transport.IsRetryableStatus(resp.StatusCode) {
				return &transport.RetryableError{Err: httpErr}
			}
			return httpErr
		}
		return json.Unmarshal(rb, &or)
	})
	if err != nil {
		return nil, err
	}
	if len(or.Choices) == 0 {
		return nil, errors.New("azure: empty choices")
	}
	return openai.FromChoice(or.Choices[0], or.Usage), nil
}

func (a *Azure) Stream(ctx context.Context, req Request) (StreamReader, error) {
	if err := a.preflight(); err != nil {
		return nil, err
	}
	body := openai.ToRequest(req, a.Model, a.MaxTokens)
	body.Stream = true

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	var resp *http.Response
	err = transport.RetryWithBackoff(ctx, 3, 0, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", a.chatURL(), bytes.NewReader(buf))
		if err != nil {
			return err
		}
		a.setHeaders(httpReq)
		resp, err = a.httpClient.Do(httpReq)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			rb, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			httpErr := fmt.Errorf("azure %d: %s", resp.StatusCode, transport.Truncate(string(rb), 500))
			if transport.IsRetryableStatus(resp.StatusCode) {
				return &transport.RetryableError{Err: httpErr}
			}
			return httpErr
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return openai.NewStream(resp.Body), nil
}
