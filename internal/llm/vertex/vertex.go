package vertex

// Vertex AI provider — Anthropic Claude served via Google Cloud's
// Vertex AI Model Garden. Wire format is Anthropic's Messages API
// with two Vertex-specific differences:
//
//   1. URL: https://{region}-aiplatform.googleapis.com/v1/projects/{project}/locations/{region}/publishers/anthropic/models/{model}:streamRawPredict
//   2. Auth: Bearer access_token from a GCP service account (no API
//      key concept on Vertex)
//   3. Body adds "anthropic_version": "vertex-2023-10-16" and DROPS
//      the "model" field (Vertex routes by URL).
//
// Streaming uses the same SSE wire format as direct Anthropic
// (data: {"type":"...",...}\n\n), so we reuse newAnthropicStream
// verbatim. Only the request setup differs.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
	"github.com/Ricardo-M-L/metis/internal/llm/cloud"
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

// Vertex implements the Anthropic-on-Vertex transport. ServiceAccount
// holds the parsed credential; TokenSource caches access tokens to
// avoid signing a JWT per request.
type Vertex struct {
	Project     string
	Region      string
	Model       string
	MaxTokens   int
	httpClient  *http.Client
	tokenSource *cloud.GCPTokenSource

	// ContextWindow override, same shape as the other providers.
	ContextWindow int
}

// NewVertex builds a Vertex AI provider from a service-account file
// path + project + region + model. Service account must have the
// `aiplatform.user` IAM role on the project for Vertex calls to
// succeed (404s otherwise).
func NewVertex(serviceAccountPath, project, region, model string, maxTokens int, timeout time.Duration) (*Vertex, error) {
	if project == "" {
		return nil, errors.New("vertex: project is required")
	}
	if region == "" {
		region = "us-central1"
	}
	if model == "" {
		return nil, errors.New("vertex: model is required (e.g. claude-sonnet-4-5@20250514)")
	}
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	data, err := os.ReadFile(serviceAccountPath)
	if err != nil {
		return nil, fmt.Errorf("vertex: read service-account file %q: %w", serviceAccountPath, err)
	}
	key, err := cloud.LoadServiceAccount(data)
	if err != nil {
		return nil, err
	}

	return &Vertex{
		Project:     project,
		Region:      region,
		Model:       model,
		MaxTokens:   maxTokens,
		httpClient:  transport.NewHTTPClient(timeout, "vertex"),
		tokenSource: cloud.NewGCPTokenSource(key, ""),
	}, nil
}

func (v *Vertex) Name() string         { return "vertex" }
func (v *Vertex) ModelID() string      { return v.Model }
func (v *Vertex) SupportsVision() bool { return anthropic.SupportsVisionModel(v.Model) }
func (v *Vertex) VisionCapability() provider.VisionCapability {
	return anthropic.VisionCapabilityForModel(v.Model)
}
func (v *Vertex) MaxContextTokens() int { return v.ContextWindow }

// endpoint renders the Vertex predict URL. `streaming` chooses between
// :rawPredict and :streamRawPredict — Vertex requires picking up
// front (different endpoints, not a request flag).
func (v *Vertex) endpoint(streaming bool) string {
	suffix := ":rawPredict"
	if streaming {
		suffix = ":streamRawPredict"
	}
	return fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic/models/%s%s",
		v.Region, v.Project, v.Region, v.Model, suffix,
	)
}

// vertexBody is the JSON shape Vertex accepts. Same as anthropicReq
// minus the `model` field plus a Vertex-specific `anthropic_version`.
// We can't reuse anthropicReq directly (no omitempty on Model) so we
// re-serialize via map.
func vertexBody(req Request, maxTokens int) (map[string]any, error) {
	a := anthropic.ToRequest(req, "", maxTokens) // model="" so it's set to "" in struct; we strip below
	buf, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, err
	}
	delete(m, "model")
	m["anthropic_version"] = "vertex-2023-10-16"
	return m, nil
}

func (v *Vertex) authHeader(ctx context.Context, r *http.Request) error {
	tok, err := v.tokenSource.Token(ctx)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	return nil
}

func (v *Vertex) Complete(ctx context.Context, req Request) (*Response, error) {
	body, err := vertexBody(req, v.MaxTokens)
	if err != nil {
		return nil, err
	}
	body["stream"] = false
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	var ar anthropic.Resp
	err = transport.RetryWithBackoff(ctx, 3, 0, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", v.endpoint(false), bytes.NewReader(buf))
		if err != nil {
			return err
		}
		if err := v.authHeader(ctx, httpReq); err != nil {
			return err
		}
		resp, err := v.httpClient.Do(httpReq)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			httpErr := fmt.Errorf("vertex %d: %s", resp.StatusCode, transport.Truncate(string(rb), 500))
			if transport.IsRetryableStatus(resp.StatusCode) {
				return &transport.RetryableError{Err: httpErr, After: transport.ParseRetryAfter(resp)}
			}
			return httpErr
		}
		return json.Unmarshal(rb, &ar)
	})
	if err != nil {
		return nil, err
	}
	return anthropic.FromResponse(ar), nil
}

func (v *Vertex) Stream(ctx context.Context, req Request) (StreamReader, error) {
	body, err := vertexBody(req, v.MaxTokens)
	if err != nil {
		return nil, err
	}
	body["stream"] = true
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	var resp *http.Response
	err = transport.RetryWithBackoff(ctx, 3, 0, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", v.endpoint(true), bytes.NewReader(buf))
		if err != nil {
			return err
		}
		if err := v.authHeader(ctx, httpReq); err != nil {
			return err
		}
		resp, err = v.httpClient.Do(httpReq)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			rb, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			httpErr := fmt.Errorf("vertex %d: %s", resp.StatusCode, transport.Truncate(string(rb), 500))
			if transport.IsRetryableStatus(resp.StatusCode) {
				return &transport.RetryableError{Err: httpErr, After: transport.ParseRetryAfter(resp)}
			}
			return httpErr
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// SSE format on Vertex matches direct Anthropic, so the existing
	// stream parser works as-is. The model id (carried via URL) means
	// nothing in the SSE events beyond echoing in usage stats.
	return anthropic.NewStream(resp.Body), nil
}

// AsAnthropicCompat exposes the Vertex provider's response unmarshal
// path for places where calling code wants to share Anthropic helpers.
// Currently unused; kept for symmetry with the Anthropic struct's own
// helper exposure.
func (v *Vertex) AsAnthropicCompat() {}

// suppress unused-import warning for strings in case the import set
// shifts under refactor.
var _ = strings.TrimSpace
