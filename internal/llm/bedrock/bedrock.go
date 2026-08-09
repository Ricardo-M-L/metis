package bedrock

// AWS Bedrock Runtime — Anthropic Claude served on AWS. Uses the same
// Anthropic Messages API body but with `anthropic_version` set to the
// Bedrock-namespaced literal, and routes via SigV4-signed POSTs to
// `bedrock-runtime.{region}.amazonaws.com`.
//
// Streaming uses AWS event-stream binary protocol — the encoded
// frames don't match SSE, so we'd need a separate parser. First cut
// here calls the SYNC InvokeModel endpoint and synthesizes a single-
// chunk stream so existing agent.Loop streaming consumers don't
// branch. Real event-stream support is a future iteration.
//
// Auth is SigV4 with static IAM credentials. STS / IRSA / web
// identity / instance profile chains are NOT auto-resolved here yet;
// users on those flows pass the temporary credentials directly via
// AWS_SESSION_TOKEN + AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY
// environment variables.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	StreamEvent  = provider.StreamEvent
	ContentBlock = provider.ContentBlock
	Message      = provider.Message
)

const (
	RoleSystem    = provider.RoleSystem
	RoleUser      = provider.RoleUser
	RoleAssistant = provider.RoleAssistant
	RoleTool      = provider.RoleTool
)

// Bedrock implements the Anthropic-on-Bedrock transport.
type Bedrock struct {
	Region    string // e.g. "us-east-1"
	Model     string // e.g. "anthropic.claude-sonnet-4-5-20250514-v1:0" — Bedrock model ARN/id
	MaxTokens int

	creds      cloud.AWSCreds
	httpClient *http.Client

	ContextWindow int
}

// NewBedrock builds a Bedrock provider. accessKey + secretKey are
// required; session token is optional (set when using STS-issued
// credentials). Region is the AWS region the model is deployed in
// (Bedrock model availability varies by region — check AWS docs).
//
// Model id should include the cross-region inference prefix when
// applicable: `us.anthropic.claude-sonnet-4-5-20250514-v1:0` for
// Claude served via the US inference profile. metis does NOT auto-
// prefix; the user supplies the full id from `metis models bedrock`.
func NewBedrock(accessKey, secretKey, sessionToken, region, model string, maxTokens int, timeout time.Duration) (*Bedrock, error) {
	if accessKey == "" {
		// Allow env-var resolution as a fallback so users who already
		// have AWS_ACCESS_KEY_ID exported don't have to re-paste.
		accessKey = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if secretKey == "" {
		secretKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	if sessionToken == "" {
		sessionToken = os.Getenv("AWS_SESSION_TOKEN")
	}
	if accessKey == "" || secretKey == "" {
		return nil, errors.New("bedrock: AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY required (set in env or [provider.bedrock] block)")
	}
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	if region == "" {
		region = "us-east-1"
	}
	if model == "" {
		return nil, errors.New("bedrock: model is required (e.g. us.anthropic.claude-sonnet-4-5-20250514-v1:0)")
	}
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Bedrock{
		Region:    region,
		Model:     model,
		MaxTokens: maxTokens,
		creds: cloud.AWSCreds{
			AccessKeyID:     accessKey,
			SecretAccessKey: secretKey,
			SessionToken:    sessionToken,
		},
		httpClient: transport.NewHTTPClient(timeout, "bedrock"),
	}, nil
}

func (b *Bedrock) Name() string          { return "bedrock" }
func (b *Bedrock) ModelID() string       { return b.Model }
func (b *Bedrock) SupportsVision() bool  { return anthropic.SupportsVisionModel(b.Model) }
func (b *Bedrock) MaxContextTokens() int { return b.ContextWindow }

// invokeURL is the synchronous Bedrock Runtime endpoint. Returns the
// streaming variant when streaming=true (kept for the future when
// metis grows an event-stream parser).
func (b *Bedrock) invokeURL(streaming bool) string {
	suffix := "invoke"
	if streaming {
		suffix = "invoke-with-response-stream"
	}
	// The model id contains colons (`...v1:0`) which must be URL-
	// encoded in the path. PathEscape handles it.
	return fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/%s",
		b.Region, url.PathEscape(b.Model), suffix)
}

// bedrockBody is Anthropic's Messages API body shape with model
// dropped (Bedrock routes by URL) and anthropic_version set to
// Bedrock's required value. Same approach as Vertex.
func bedrockBody(req Request, maxTokens int) (map[string]any, error) {
	a := anthropic.ToRequest(req, "", maxTokens)
	buf, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, err
	}
	delete(m, "model")
	m["anthropic_version"] = "bedrock-2023-05-31"
	return m, nil
}

// signedRequest builds the http.Request, signs it with SigV4, and
// returns it ready to send. Factored out so Complete and (future)
// Stream share the signing path.
func (b *Bedrock) signedRequest(ctx context.Context, urlStr string, payload []byte) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", urlStr, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if err := cloud.SignV4(httpReq, payload, b.creds, b.Region, "bedrock", time.Now()); err != nil {
		return nil, err
	}
	return httpReq, nil
}

func (b *Bedrock) Complete(ctx context.Context, req Request) (*Response, error) {
	body, err := bedrockBody(req, b.MaxTokens)
	if err != nil {
		return nil, err
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	var ar anthropic.Resp
	err = transport.RetryWithBackoff(ctx, 3, 0, func() error {
		httpReq, err := b.signedRequest(ctx, b.invokeURL(false), buf)
		if err != nil {
			return err
		}
		resp, err := b.httpClient.Do(httpReq)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			httpErr := fmt.Errorf("bedrock %d: %s", resp.StatusCode, transport.Truncate(string(rb), 500))
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

// Stream uses the synchronous endpoint and synthesizes a one-event
// stream around the Complete response. The agent loop's StreamReader
// consumer doesn't care that there's no progressive output — a single
// "TextDelta(<full body>) + Done" pair is a valid (just chunkier)
// stream.
//
// Real Bedrock streaming uses AWS event-stream binary frames; supporting
// it requires a separate decoder. Tracked as future work.
func (b *Bedrock) Stream(ctx context.Context, req Request) (StreamReader, error) {
	resp, err := b.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	return newSyntheticStream(resp), nil
}

// syntheticStream wraps a fully-realized Response in the StreamReader
// interface. Used by providers that don't implement real streaming
// yet — letting the rest of the agent loop treat them uniformly.
//
// Event shape mirrors what the Anthropic streaming parser emits so
// the consumer (agent.Loop) doesn't branch on "is this a real stream
// or synthetic". A tool-use block expands to the canonical 3-event
// sequence (tool_use_start → tool_input_delta(full JSON) → tool_use_stop);
// a text block becomes a single text_delta.
type syntheticStream struct {
	resp    *Response
	pending []StreamEvent
	done    bool
}

func newSyntheticStream(r *Response) *syntheticStream {
	s := &syntheticStream{resp: r}
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			s.pending = append(s.pending, StreamEvent{Type: "text_delta", TextDelta: c.Text})
		case "tool_use":
			// Anthropic-stream-equivalent 3-event sequence.
			inputJSON, _ := json.Marshal(c.ToolInput)
			s.pending = append(s.pending,
				StreamEvent{Type: "tool_use_start", ToolUseID: c.ToolUseID, ToolName: c.ToolName},
				StreamEvent{Type: "tool_input_delta", ToolUseID: c.ToolUseID, InputDelta: string(inputJSON)},
				StreamEvent{Type: "tool_use_stop", ToolUseID: c.ToolUseID, InputDelta: string(inputJSON)},
			)
		}
	}
	// Terminating message_stop carries usage + stop reason.
	s.pending = append(s.pending, StreamEvent{
		Type:                     "message_stop",
		InputTokens:              r.InputTokens,
		OutputTokens:             r.OutputTokens,
		CacheCreationInputTokens: r.CacheCreationInputTokens,
		CacheReadInputTokens:     r.CacheReadInputTokens,
		StopReason:               r.StopReason,
	})
	return s
}

func (s *syntheticStream) Close() error { return nil }

func (s *syntheticStream) Recv() (StreamEvent, error) {
	if s.done {
		return StreamEvent{}, io.EOF
	}
	if len(s.pending) == 0 {
		s.done = true
		return StreamEvent{}, io.EOF
	}
	ev := s.pending[0]
	s.pending = s.pending[1:]
	if ev.Type == "message_stop" {
		s.done = true
	}
	return ev, nil
}
