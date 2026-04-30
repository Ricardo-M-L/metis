package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Anthropic implements Provider against the Messages API.
// We hand-roll the HTTP layer to keep the dependency footprint minimal
// and to control SSE parsing precisely.
type Anthropic struct {
	APIKey    string
	BaseURL   string
	Model     string
	MaxTokens int
	Beta      string
	// ContextWindow, when > 0, overrides the model-prefix lookup in
	// MaxContextTokens(). Lets users on Anthropic-compatible third-party
	// gateways (MiniMax, OpenRouter, ...) declare the real window so
	// auto-compaction triggers at the right threshold.
	ContextWindow int
	httpClient    *http.Client
}

func NewAnthropic(apiKey, baseURL, model string, maxTokens int, timeout time.Duration, beta string) *Anthropic {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	if model == "" {
		model = "claude-opus-4-7"
	}
	if maxTokens == 0 {
		maxTokens = 8192
	}
	return &Anthropic{
		APIKey:    apiKey,
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Model:     model,
		MaxTokens: maxTokens,
		Beta:      beta,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// MaxContextTokens returns the effective context window for compaction.
// Precedence: explicit ContextWindow override > model-prefix lookup.
//
// For MiniMax-M* (Anthropic-compatible gateway) we default to 192k —
// MiniMax M2 publishes a 192k window, but their API's effective request
// budget is lower, so a too-large default lets requests overflow before
// compaction fires (the user hit "context window exceeds limit (2013)"
// at runtime). Set [provider.anthropic].context_window in config.toml
// if your gateway has a tighter cap.
func (a *Anthropic) MaxContextTokens() int {
	if a.ContextWindow > 0 {
		return a.ContextWindow
	}
	switch {
	case strings.HasPrefix(a.Model, "claude-opus"):
		return 200000
	case strings.HasPrefix(a.Model, "claude-sonnet"):
		return 200000
	case strings.HasPrefix(a.Model, "claude-haiku"):
		return 200000
	case strings.HasPrefix(a.Model, "MiniMax"), strings.HasPrefix(a.Model, "minimax"):
		return 192000
	default:
		return 200000 // safe default for unknown Anthropic-compatible models
	}
}

func (a *Anthropic) Name() string { return "anthropic" }

// --- request shapes (Anthropic-native) ---

type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

type anthropicContent struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   any            `json:"content,omitempty"` // string or []block in tool_result
	IsError   bool           `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicThinking is Anthropic's extended-thinking config block.
// We only ever send Type="enabled" with a positive BudgetTokens; nil = omit.
type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicReq struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	Thinking      *anthropicThinking `json:"thinking,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
}

type anthropicResp struct {
	ID         string             `json:"id"`
	Role       string             `json:"role"`
	Type       string             `json:"type"`
	Content    []anthropicContent `json:"content"`
	StopReason string             `json:"stop_reason"`
	Model      string             `json:"model"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// --- conversion helpers ---

func toAnthropic(req Request, model string, maxTokens int) anthropicReq {
	// MaxTokens precedence: per-request override > provider default.
	// The override path is what /fast uses to halve budget without
	// touching the underlying client config.
	mt := maxTokens
	if req.MaxTokens > 0 {
		mt = req.MaxTokens
	}
	out := anthropicReq{
		Model:         model,
		MaxTokens:     mt,
		System:        req.System,
		Stream:        req.Stream,
		StopSequences: req.StopSequences,
	}
	if req.Temperature > 0 {
		t := req.Temperature
		out.Temperature = &t
	}
	// Extended thinking: only set when caller explicitly asked for an
	// effort level. Some Anthropic models (e.g. older Sonnet) reject
	// the field entirely, so silence-by-default is safer than an
	// always-on small budget.
	if budget := req.Effort.BudgetTokens(); budget > 0 {
		out.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, anthropicTool{
			Name: t.Name, Description: t.Description, InputSchema: t.InputSchema,
		})
	}
	for _, m := range req.Messages {
		am := anthropicMessage{Role: string(m.Role)}
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				am.Content = append(am.Content, anthropicContent{Type: "text", Text: c.Text})
			case "tool_use":
				am.Content = append(am.Content, anthropicContent{
					Type: "tool_use", ID: c.ToolUseID, Name: c.ToolName, Input: c.ToolInput,
				})
			case "tool_result":
				am.Content = append(am.Content, anthropicContent{
					Type: "tool_result", ToolUseID: c.ToolUseID, Content: c.ToolResult, IsError: c.IsError,
				})
			}
		}
		out.Messages = append(out.Messages, am)
	}
	return out
}

func fromAnthropic(resp anthropicResp) *Response {
	out := &Response{
		StopReason:   resp.StopReason,
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
	}
	for _, c := range resp.Content {
		switch c.Type {
		case "text":
			out.Content = append(out.Content, ContentBlock{Type: "text", Text: c.Text})
		case "tool_use":
			out.Content = append(out.Content, ContentBlock{
				Type: "tool_use", ToolUseID: c.ID, ToolName: c.Name, ToolInput: c.Input,
			})
		}
	}
	return out
}

// --- Provider impl ---

func (a *Anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	if a.APIKey == "" {
		return nil, fmt.Errorf("API key not configured. Set ANTHROPIC_API_KEY environment variable or configure in ~/.metis/config.toml")
	}
	body := toAnthropic(req, a.Model, a.MaxTokens)
	body.Stream = false

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	var ar anthropicResp
	err = retryWithBackoff(ctx, 3, 0, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/v1/messages", bytes.NewReader(buf))
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
			httpErr := fmt.Errorf("anthropic %d: %s", resp.StatusCode, truncate(string(rb), 500))
			if isRetryableStatus(resp.StatusCode) {
				return &RetryableError{Err: httpErr}
			}
			return httpErr
		}
		if err := json.Unmarshal(rb, &ar); err != nil {
			return fmt.Errorf("decode anthropic response: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return fromAnthropic(ar), nil
}

func (a *Anthropic) Stream(ctx context.Context, req Request) (StreamReader, error) {
	if a.APIKey == "" {
		return nil, fmt.Errorf("API key not configured. Set ANTHROPIC_API_KEY environment variable or configure in ~/.metis/config.toml")
	}
	body := toAnthropic(req, a.Model, a.MaxTokens)
	body.Stream = true

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	// Retry the initial request (DNS / 429 / 5xx). Once we have a streaming
	// body we don't retry — partial SSE consumption can't be re-played.
	var resp *http.Response
	err = retryWithBackoff(ctx, 3, 0, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/v1/messages", bytes.NewReader(buf))
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
			httpErr := fmt.Errorf("anthropic %d: %s", resp.StatusCode, truncate(string(rb), 500))
			if isRetryableStatus(resp.StatusCode) {
				return &RetryableError{Err: httpErr}
			}
			return httpErr
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return newAnthropicStream(resp.Body), nil
}

// isRetryableStatus picks the HTTP status codes that can plausibly be retried.
// 429 (rate limit), 503 (overloaded), 529 (Anthropic-specific overloaded),
// 502/504 (gateway hiccup). We deliberately exclude 500 — it can mean the
// request itself is malformed.
func isRetryableStatus(code int) bool {
	switch code {
	case 429, 502, 503, 504, 529:
		return true
	}
	return false
}

func (a *Anthropic) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-api-key", a.APIKey)
	r.Header.Set("anthropic-version", "2023-06-01")
	if a.Beta != "" {
		r.Header.Set("anthropic-beta", a.Beta)
	}
}

// --- SSE stream ---

type anthropicStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	// transient state for tool_use blocks (we accumulate input_json_delta)
	currentBlocks map[int]*streamBlock
}

type streamBlock struct {
	Type      string
	ToolUseID string
	ToolName  string
	JSONBuf   strings.Builder
}

func newAnthropicStream(body io.ReadCloser) *anthropicStream {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	return &anthropicStream{
		body:          body,
		scanner:       sc,
		currentBlocks: make(map[int]*streamBlock),
	}
}

func (s *anthropicStream) Close() error { return s.body.Close() }

func (s *anthropicStream) Recv() (StreamEvent, error) {
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			return StreamEvent{Type: "message_stop"}, io.EOF
		}

		var env struct {
			Type         string          `json:"type"`
			Index        int             `json:"index"`
			Delta        json.RawMessage `json:"delta"`
			ContentBlock json.RawMessage `json:"content_block"`
			Message      json.RawMessage `json:"message"`
			Usage        struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &env); err != nil {
			continue
		}

		switch env.Type {
		case "message_start":
			// usage info available; emit empty event with token counts
			var msg struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			_ = json.Unmarshal(env.Message, &msg)
			return StreamEvent{Type: "message_start", InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens}, nil
		case "content_block_start":
			var cb struct {
				Type  string         `json:"type"`
				ID    string         `json:"id"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			}
			_ = json.Unmarshal(env.ContentBlock, &cb)
			s.currentBlocks[env.Index] = &streamBlock{Type: cb.Type, ToolUseID: cb.ID, ToolName: cb.Name}
			if cb.Type == "tool_use" {
				return StreamEvent{Type: "tool_use_start", ToolUseID: cb.ID, ToolName: cb.Name}, nil
			}
			continue
		case "content_block_delta":
			var d struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
			}
			_ = json.Unmarshal(env.Delta, &d)
			blk := s.currentBlocks[env.Index]
			if d.Type == "text_delta" {
				return StreamEvent{Type: "text_delta", TextDelta: d.Text}, nil
			}
			// Extended-thinking: Anthropic emits the reasoning trace as
			// thinking_delta blocks separately from text. Forward them so
			// the UI can render dim/italic alongside the final answer.
			// Signature deltas (cryptographic block signature) are
			// internal — drop them rather than streaming binary noise.
			if d.Type == "thinking_delta" {
				return StreamEvent{Type: "thinking_delta", TextDelta: d.Thinking}, nil
			}
			if d.Type == "signature_delta" {
				continue
			}
			if d.Type == "input_json_delta" && blk != nil {
				blk.JSONBuf.WriteString(d.PartialJSON)
				return StreamEvent{Type: "tool_input_delta", ToolUseID: blk.ToolUseID, InputDelta: d.PartialJSON}, nil
			}
			continue
		case "content_block_stop":
			blk := s.currentBlocks[env.Index]
			if blk != nil && blk.Type == "tool_use" {
				ev := StreamEvent{Type: "tool_use_stop", ToolUseID: blk.ToolUseID, InputDelta: blk.JSONBuf.String()}
				delete(s.currentBlocks, env.Index)
				return ev, nil
			}
			delete(s.currentBlocks, env.Index)
			continue
		case "message_delta":
			var d struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(env.Delta, &d)
			// Forward both input + output tokens. Anthropic native usually
			// reports input_tokens at message_start and only output_tokens
			// at message_delta, but Anthropic-compatible gateways are
			// inconsistent — MiniMax in particular reports the input
			// count here, not at the start. Carrying both means the
			// downstream tracker doesn't end up with `in == 0` on those
			// providers and the bottom-right counter actually moves.
			return StreamEvent{
				Type:         "message_delta",
				StopReason:   d.StopReason,
				InputTokens:  env.Usage.InputTokens,
				OutputTokens: env.Usage.OutputTokens,
			}, nil
		case "message_stop":
			return StreamEvent{Type: "message_stop"}, io.EOF
		case "error":
			return StreamEvent{Type: "error", Err: errors.New(string(payload))}, nil
		}
	}
	if err := s.scanner.Err(); err != nil {
		return StreamEvent{Type: "error", Err: err}, err
	}
	return StreamEvent{Type: "message_stop"}, io.EOF
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
