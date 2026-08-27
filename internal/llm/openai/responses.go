package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/sse"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// Responses implements Provider against the OpenAI Responses API
// (POST /v1/responses, streaming over SSE). This is the second wire
// protocol beside /chat/completions for endpoints that expose it —
// api.openai.com, Azure "responses" deployments, and gateways that
// proxy the Responses shape (xAI, some open-source servers). DeepSeek's
// official API does NOT serve /v1/responses, so this provider is for
// other origins.
//
// The event vocabulary maps onto the same provider-neutral StreamEvents
// the chat/completions path emits, so the agent loop and TUI need zero
// changes:
//
//	response.output_item.added(function_call)        → tool_use_start
//	response.function_call_arguments.delta           → tool_input_delta
//	response.output_item.done(function_call)         → tool_use_stop (full args resync)
//	response.output_text.delta                       → text_delta
//	response.reasoning_summary_text.delta            → thinking_delta
//	response.completed / response.incomplete         → message_delta (stop + usage)
//	response.failed / error                          → error
type Responses struct {
	APIKey      string
	BaseURL     string
	Model       string
	MaxTokens   int
	Temperature float64
	// ContextWindow, when > 0, overrides the default in MaxContextTokens().
	ContextWindow int
	httpClient    *http.Client
}

// NewResponses builds a Responses-API provider. baseURL must NOT include
// the trailing path (e.g. "https://api.openai.com/v1").
func NewResponses(apiKey, baseURL, model string, maxTokens int, timeout time.Duration, temperature float64) *Responses {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	if maxTokens == 0 {
		maxTokens = 4096
	}
	return &Responses{
		APIKey:      apiKey,
		BaseURL:     strings.TrimRight(baseURL, "/"),
		Model:       model,
		MaxTokens:   maxTokens,
		Temperature: temperature,
		httpClient:  transport.NewHTTPClient(timeout, "openai-responses"),
	}
}

func (r *Responses) Name() string    { return "openai-responses" }
func (r *Responses) ModelID() string { return r.Model }

// The Responses request wire has no history item for Metis reasoning blocks;
// both plaintext and redacted thinking are skipped by toResponsesRequest.
func (r *Responses) ContextIncludesAssistantBlock(block provider.ContentBlock) bool {
	return block.Type != "thinking" && block.Type != "redacted_thinking"
}

func (r *Responses) MaxContextTokens() int {
	if r.ContextWindow > 0 {
		return r.ContextWindow
	}
	return 200_000 // Responses-served o-series / frontier models
}

// ---------- request building ----------

type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type responsesInputItem struct {
	Type string `json:"type"`
	Role string `json:"role,omitempty"`
	// message item
	Content []responsesContentPart `json:"content,omitempty"`
	// function_call item
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call_output item
	Output string `json:"output,omitempty"`
}

type responsesContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesRequest struct {
	Model           string               `json:"model"`
	Instructions    string               `json:"instructions,omitempty"`
	Input           []responsesInputItem `json:"input"`
	Tools           []responsesTool      `json:"tools,omitempty"`
	MaxOutputTokens int                  `json:"max_output_tokens,omitempty"`
	Temperature     float64              `json:"temperature,omitempty"`
	Stream          bool                 `json:"stream,omitempty"`
	Reasoning       *responsesReasoning  `json:"reasoning,omitempty"`
	Store           bool                 `json:"store"`
}

type responsesReasoning struct {
	Effort string `json:"effort"`
}

// buildResponsesRequest flattens metis's Message/ContentBlock history into
// the Responses `input` item list. Assistant tool_use blocks become
// `function_call` items; user tool_result blocks become
// `function_call_output` items; text blocks become message items.
func (r *Responses) buildResponsesRequest(req provider.Request) (*responsesRequest, error) {
	out := &responsesRequest{
		Model:           r.Model,
		Instructions:    req.System,
		MaxOutputTokens: r.MaxTokens,
		Temperature:     r.Temperature,
		Stream:          req.Stream,
		Store:           false,
	}
	if req.MaxTokens > 0 {
		out.MaxOutputTokens = req.MaxTokens
	}
	if req.Effort != "" {
		out.Reasoning = &responsesReasoning{Effort: string(req.Effort)}
	}
	if len(req.SystemSections) > 0 {
		var sb strings.Builder
		for _, sec := range req.SystemSections {
			if sec.Body == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(sec.Body)
		}
		out.Instructions = sb.String()
	}

	for _, m := range req.Messages {
		switch m.Role {
		case provider.RoleSystem:
			// System messages are folded into instructions (defensive:
			// callers normally use Request.System).
			if len(m.Content) > 0 {
				for _, b := range m.Content {
					if b.Type == "text" && b.Text != "" {
						if out.Instructions != "" {
							out.Instructions += "\n\n"
						}
						out.Instructions += b.Text
					}
				}
			}
		case provider.RoleUser:
			var parts []responsesContentPart
			flushParts := func() {
				if len(parts) > 0 {
					out.Input = append(out.Input, responsesInputItem{Type: "message", Role: "user", Content: parts})
					parts = nil
				}
			}
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					if b.Text != "" {
						parts = append(parts, responsesContentPart{Type: "input_text", Text: b.Text})
					}
				case "tool_result":
					flushParts()
					// Only function-call tools round-trip; any other
					// tool_result kind has no Responses wire item.
					out.Input = append(out.Input, responsesInputItem{
						Type:   "function_call_output",
						CallID: b.ToolUseID,
						Output: b.ToolResult,
					})
				case "image":
					// Responses speaks `input_image` parts; metis images ride
					// image blocks with base64 data. Pass through as-is.
					if b.MediaType != "" && b.Data != "" {
						parts = append(parts, responsesContentPart{Type: "image", Text: "data:" + b.MediaType + ";base64," + b.Data})
					}
				}
			}
			flushParts()
		case provider.RoleAssistant:
			var parts []responsesContentPart
			flushParts := func() {
				if len(parts) > 0 {
					out.Input = append(out.Input, responsesInputItem{Type: "message", Role: "assistant", Content: parts})
					parts = nil
				}
			}
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					if b.Text != "" {
						parts = append(parts, responsesContentPart{Type: "output_text", Text: b.Text})
					}
				case "tool_use":
					flushParts()
					args := "{}"
					if len(b.ToolInput) > 0 {
						if buf, err := json.Marshal(b.ToolInput); err == nil {
							args = string(buf)
						}
					}
					out.Input = append(out.Input, responsesInputItem{
						Type:      "function_call",
						CallID:    b.ToolUseID,
						Name:      b.ToolName,
						Arguments: args,
					})
				case "thinking", "redacted_thinking":
					// Reasoning history does not round-trip on the Responses
					// wire (no reasoning item type); it is skipped.
				}
			}
			flushParts()
		}
	}

	for _, t := range req.Tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out.Tools = append(out.Tools, responsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
		})
	}
	return out, nil
}

func (r *Responses) setHeaders(h *http.Request) {
	h.Header.Set("Content-Type", "application/json")
	h.Header.Set("Authorization", "Bearer "+r.APIKey)
}

// ---------- streamed response parsing ----------

// responsesStream is the StreamReader for a live /v1/responses SSE body.
// Recv pulls frames lazily and converts them into the provider-neutral
// StreamEvent vocabulary; pending events queue when one frame maps to
// several (e.g. output_item.done also flushes tool_use_stop).
type responsesStream struct {
	sse     *sse.Reader
	body    io.Closer
	pending []provider.StreamEvent
	done    bool
	sentEnd bool
}

func (s *responsesStream) Close() error { return s.body.Close() }

func (s *responsesStream) Recv() (provider.StreamEvent, error) {
	if len(s.pending) > 0 {
		ev := s.pending[0]
		s.pending = s.pending[1:]
		return ev, nil
	}
	for {
		if s.done {
			if !s.sentEnd {
				s.sentEnd = true
				return provider.StreamEvent{Type: "message_stop"}, io.EOF
			}
			return provider.StreamEvent{}, io.EOF
		}
		frame, err := s.sse.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !s.sentEnd {
					s.sentEnd = true
					return provider.StreamEvent{Type: "message_stop"}, io.EOF
				}
			}
			if !errors.Is(err, io.EOF) {
				return provider.StreamEvent{Type: "error", Err: err}, err
			}
			return provider.StreamEvent{}, io.EOF
		}
		if frame.Data == "" || frame.Data == "[DONE]" {
			continue
		}
		var env struct {
			Type     string `json:"type"`
			Sequence string `json:"sequence_number"`
			Delta    string `json:"delta"`
			ItemID   string `json:"item_id"`
			Item     struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Response *struct {
				Status string `json:"status"`
				Error  *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
				IncompleteDetails *struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
				Usage *struct {
					InputTokens        int `json:"input_tokens"`
					OutputTokens       int `json:"output_tokens"`
					TotalTokens        int `json:"total_tokens"`
					InputTokensDetails *struct {
						CachedTokens int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
				Output []struct {
					Type      string `json:"type"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"output"`
			} `json:"response"`
			Error *struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(frame.Data), &env); err != nil {
			continue // non-JSON keep-alive line
		}
		switch env.Type {
		case "response.output_item.added":
			if env.Item.Type == "function_call" {
				s.pending = append(s.pending, provider.StreamEvent{
					Type:      "tool_use_start",
					ToolUseID: env.Item.CallID,
					ToolName:  env.Item.Name,
				})
			}
		case "response.function_call_arguments.delta":
			if env.Delta != "" {
				s.pending = append(s.pending, provider.StreamEvent{Type: "tool_input_delta", ToolUseID: env.ItemID, InputDelta: env.Delta})
			}
		case "response.output_item.done":
			if env.Item.Type == "function_call" {
				s.pending = append(s.pending, provider.StreamEvent{
					Type:       "tool_use_stop",
					ToolUseID:  env.Item.CallID,
					InputDelta: env.Item.Arguments, // full args resync (authoritative)
				})
			}
		case "response.output_text.delta":
			if env.Delta != "" {
				s.pending = append(s.pending, provider.StreamEvent{Type: "text_delta", TextDelta: env.Delta})
			}
		case "response.reasoning_summary_text.delta":
			if env.Delta != "" {
				s.pending = append(s.pending, provider.StreamEvent{Type: "thinking_delta", TextDelta: env.Delta})
			}
		case "response.completed":
			ev := provider.StreamEvent{Type: "message_delta", StopReason: "end_turn"}
			if env.Response != nil {
				if env.Response.Usage != nil {
					cacheRead := 0
					if env.Response.Usage.InputTokensDetails != nil {
						cacheRead = env.Response.Usage.InputTokensDetails.CachedTokens
					}
					ev.InputTokens, ev.CacheReadInputTokens = normalizeInputUsage(
						env.Response.Usage.InputTokens,
						cacheRead,
					)
					ev.OutputTokens = env.Response.Usage.OutputTokens
				}
				if hasFunctionCall(env.Response.Output) {
					ev.StopReason = "tool_use"
				}
			}
			s.pending = append(s.pending, ev)
			s.done = true
		case "response.incomplete":
			ev := provider.StreamEvent{Type: "message_delta", StopReason: "max_tokens"}
			if env.Response != nil && env.Response.IncompleteDetails != nil && env.Response.IncompleteDetails.Reason == "content_filter" {
				ev.StopReason = "stop_sequence"
			}
			if env.Response != nil && env.Response.Usage != nil {
				cacheRead := 0
				if env.Response.Usage.InputTokensDetails != nil {
					cacheRead = env.Response.Usage.InputTokensDetails.CachedTokens
				}
				ev.InputTokens, ev.CacheReadInputTokens = normalizeInputUsage(
					env.Response.Usage.InputTokens,
					cacheRead,
				)
				ev.OutputTokens = env.Response.Usage.OutputTokens
			}
			s.pending = append(s.pending, ev)
			s.done = true
		case "response.failed":
			msg := "responses request failed"
			if env.Response != nil && env.Response.Error != nil {
				if env.Response.Error.Message != "" {
					msg = env.Response.Error.Message
				}
			}
			s.pending = append(s.pending, provider.StreamEvent{Type: "error", Err: errors.New(msg)})
			s.done = true
		case "error":
			msg := "responses stream error"
			if env.Error != nil && env.Error.Message != "" {
				msg = env.Error.Message
			}
			s.pending = append(s.pending, provider.StreamEvent{Type: "error", Err: errors.New(msg)})
			s.done = true
		}
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
	}
}

func hasFunctionCall(output []struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}) bool {
	for _, o := range output {
		if o.Type == "function_call" {
			return true
		}
	}
	return false
}

// Stream POSTs a streaming /v1/responses request and returns the event
// reader. The body stays open until Close().
func (r *Responses) Stream(ctx context.Context, req provider.Request) (provider.StreamReader, error) {
	if r.APIKey == "" {
		return nil, fmt.Errorf("API key not configured. Set OPENAI_API_KEY or configure in ~/.metis/config.toml")
	}
	body, err := r.buildResponsesRequest(req)
	if err != nil {
		return nil, err
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var resp *http.Response
	var lastBody string
	err = transport.RetryWithBackoff(ctx, 3, 0, func() error {
		lastBody = ""
		httpReq, err := http.NewRequestWithContext(ctx, "POST", r.BaseURL+"/responses", bytes.NewReader(buf))
		if err != nil {
			return err
		}
		r.setHeaders(httpReq)
		resp, err = r.httpClient.Do(httpReq)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			rb, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				return readErr
			}
			lastBody = string(rb)
			return fmt.Errorf("responses %d: %s", resp.StatusCode, transport.Truncate(lastBody, 500))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &responsesStream{sse: sse.NewReader(resp.Body), body: resp.Body}, nil
}

// ---------- non-streamed completion ----------

type responsesComplete struct {
	Output []responsesOutputItem `json:"output"`
	Usage  *struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type responsesOutputItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Output    string `json:"output"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
}

// Complete issues a non-streamed /v1/responses call and folds the output
// items back into ContentBlocks. Tool calls are NOT executed here — like
// the chat/completions Complete, this is the aggregate view for callers
// that don't consume the stream.
func (r *Responses) Complete(ctx context.Context, req provider.Request) (*provider.Response, error) {
	if r.APIKey == "" {
		return nil, fmt.Errorf("API key not configured. Set OPENAI_API_KEY or configure in ~/.metis/config.toml")
	}
	body, err := r.buildResponsesRequest(req)
	if err != nil {
		return nil, err
	}
	body.Stream = false
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", r.BaseURL+"/responses", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	r.setHeaders(httpReq)
	resp, err := r.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("responses %d: %s", resp.StatusCode, transport.Truncate(string(raw), 500))
	}
	var out responsesComplete
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("responses parse: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return nil, errors.New(out.Error.Message)
	}
	result := &provider.Response{}
	var textParts []responsesContentPart
	flushText := func() {
		var sb strings.Builder
		for _, p := range textParts {
			sb.WriteString(p.Text)
		}
		if sb.Len() > 0 {
			result.Content = append(result.Content, provider.ContentBlock{Type: "text", Text: sb.String()})
		}
		textParts = nil
	}
	for _, item := range out.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					textParts = append(textParts, responsesContentPart{Type: "output_text", Text: part.Text})
				}
			}
		case "reasoning":
			for _, part := range item.Summary {
				if part.Type == "summary_text" && part.Text != "" {
					result.Content = append(result.Content, provider.ContentBlock{Type: "thinking", Text: part.Text})
				}
			}
		case "function_call":
			flushText()
			input := map[string]any{}
			if item.Arguments != "" {
				_ = json.Unmarshal([]byte(item.Arguments), &input)
			}
			result.Content = append(result.Content, provider.ContentBlock{
				Type:      "tool_use",
				ToolUseID: item.CallID,
				ToolName:  item.Name,
				ToolInput: input,
			})
		}
	}
	flushText()

	stop := "end_turn"
	for _, item := range out.Output {
		if item.Type == "function_call" {
			stop = "tool_use"
			break
		}
	}
	if out.Status == "incomplete" && out.IncompleteDetails != nil && out.IncompleteDetails.Reason == "max_output_tokens" {
		stop = "max_tokens"
	}
	result.StopReason = stop
	if out.Usage != nil {
		cacheRead := 0
		if out.Usage.InputTokensDetails != nil {
			cacheRead = out.Usage.InputTokensDetails.CachedTokens
		}
		result.InputTokens, result.CacheReadInputTokens = normalizeInputUsage(
			out.Usage.InputTokens,
			cacheRead,
		)
		result.OutputTokens = out.Usage.OutputTokens
	}
	return result, nil
}
