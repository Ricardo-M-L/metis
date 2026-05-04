// Package openai implements the OpenAI Chat Completions API
// (api.openai.com/v1/chat/completions and OpenAI-compatible gateways:
// DeepSeek, Together, Groq, Cerebras, MiniMax /v1, Ollama …). Used
// directly when transport=openai_chat, and consumed by the azure
// subpackage which runs the same wire format with deployment-routed URLs.
package openai

import (
	"bufio"
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

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	pubLLM "github.com/Ricardo-M-L/metis/pkg/llm"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

type (
	Request      = provider.Request
	Response     = provider.Response
	StreamReader = provider.StreamReader
	StreamEvent  = provider.StreamEvent
	Message      = provider.Message
	ContentBlock = provider.ContentBlock
	ToolSpec     = provider.ToolSpec
	Effort       = pubLLM.Effort
)

const (
	RoleSystem    = provider.RoleSystem
	RoleUser      = provider.RoleUser
	RoleAssistant = provider.RoleAssistant
	RoleTool      = provider.RoleTool

	EffortDefault = pubLLM.EffortDefault
	EffortLow     = pubLLM.EffortLow
	EffortMedium  = pubLLM.EffortMedium
	EffortHigh    = pubLLM.EffortHigh
)

var dbgOpenAI = os.Getenv("METIS_DEBUG_OPENAI") == "1"

// OpenAI implements Provider against /v1/chat/completions.
// Compatible with any OpenAI-Chat-style endpoint (Together, Groq, Ollama, etc.)
// by overriding BaseURL.
type OpenAI struct {
	APIKey      string
	BaseURL     string
	Model       string
	MaxTokens   int
	Temperature float64
	// ContextWindow, when > 0, overrides the model-prefix lookup in
	// MaxContextTokens(). Useful for OpenAI-compatible gateways
	// (Together, Groq, Ollama) where the served model is a fine-tune
	// whose name doesn't match a known prefix.
	ContextWindow int
	httpClient    *http.Client
}

func New(apiKey, baseURL, model string, maxTokens int, timeout time.Duration, temperature float64) *OpenAI {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	if maxTokens == 0 {
		maxTokens = 4096
	}
	return &OpenAI{
		APIKey:      apiKey,
		BaseURL:     strings.TrimRight(baseURL, "/"),
		Model:       model,
		MaxTokens:   maxTokens,
		Temperature: temperature,
		httpClient:  &http.Client{Timeout: timeout},
	}
}

func (o *OpenAI) MaxContextTokens() int {
	if o.ContextWindow > 0 {
		return o.ContextWindow
	}
	switch {
	case strings.HasPrefix(o.Model, "gpt-4o"):
		return 128000
	case strings.HasPrefix(o.Model, "gpt-4-turbo"):
		return 128000
	case strings.HasPrefix(o.Model, "gpt-4"):
		return 128000 // gpt-4-32k
	case strings.HasPrefix(o.Model, "gpt-3.5-turbo"):
		return 16385
	default:
		return 128000 // safe default
	}
}

func (o *OpenAI) Name() string { return "openai" }

// --- request shapes ---

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type oaiToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

type oaiReq struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	Tools       []oaiTool    `json:"tools,omitempty"`
	Stream      bool         `json:"stream,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	// ReasoningEffort is recognized by OpenAI o1 / o3 / GPT-5-style
	// reasoning models (and forwarded by most OpenAI-compat gateways).
	// Empty string means: don't send the field. Non-reasoning models
	// generally ignore it but a few error out, so omit by default.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	StreamOptions   *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

type oaiChoice struct {
	Index        int        `json:"index"`
	Message      oaiMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type oaiResp struct {
	Choices []oaiChoice `json:"choices"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// --- conversion ---

func toOpenAI(req Request, model string, maxTokens int) oaiReq {
	mt := maxTokens
	if req.MaxTokens > 0 {
		mt = req.MaxTokens
	}
	out := oaiReq{
		Model:           model,
		MaxTokens:       mt,
		Stream:          req.Stream,
		ReasoningEffort: req.Effort.OpenAI(),
	}
	if req.Temperature > 0 {
		t := req.Temperature
		out.Temperature = &t
	}
	if req.Stream {
		out.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}
	if req.System != "" {
		out.Messages = append(out.Messages, oaiMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			// User message can contain text + tool_results (we represent tool_results
			// as separate role="tool" messages in OpenAI dialect).
			var text strings.Builder
			var toolMsgs []oaiMessage
			for _, c := range m.Content {
				switch c.Type {
				case "text":
					if text.Len() > 0 {
						text.WriteByte('\n')
					}
					text.WriteString(c.Text)
				case "tool_result":
					toolMsgs = append(toolMsgs, oaiMessage{
						Role: "tool", ToolCallID: c.ToolUseID, Content: c.ToolResult,
					})
				}
			}
			// IMPORTANT: tool messages must come BEFORE the next user text in OpenAI's
			// turn-taking model. We emit tool messages first, then any user text.
			out.Messages = append(out.Messages, toolMsgs...)
			if text.Len() > 0 {
				out.Messages = append(out.Messages, oaiMessage{Role: "user", Content: text.String()})
			}
		case RoleAssistant:
			am := oaiMessage{Role: "assistant"}
			var text strings.Builder
			for _, c := range m.Content {
				switch c.Type {
				case "text":
					if text.Len() > 0 {
						text.WriteByte('\n')
					}
					text.WriteString(c.Text)
				case "tool_use":
					argsJSON, _ := json.Marshal(c.ToolInput)
					tc := oaiToolCall{ID: c.ToolUseID, Type: "function"}
					tc.Function.Name = c.ToolName
					tc.Function.Arguments = string(argsJSON)
					am.ToolCalls = append(am.ToolCalls, tc)
				}
			}
			am.Content = text.String()
			out.Messages = append(out.Messages, am)
		}
	}
	for _, t := range req.Tools {
		var ot oaiTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.InputSchema
		out.Tools = append(out.Tools, ot)
	}
	return out
}

func fromOpenAIChoice(c oaiChoice, usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}) *Response {
	out := &Response{
		StopReason:   mapOAIStop(c.FinishReason),
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
	}
	if c.Message.Content != "" {
		out.Content = append(out.Content, ContentBlock{Type: "text", Text: c.Message.Content})
	}
	for _, tc := range c.Message.ToolCalls {
		var input map[string]any
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
		out.Content = append(out.Content, ContentBlock{
			Type: "tool_use", ToolUseID: tc.ID, ToolName: tc.Function.Name, ToolInput: input,
		})
	}
	return out
}

func mapOAIStop(s string) string {
	switch s {
	case "tool_calls":
		return "tool_use"
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	default:
		return s
	}
}

// --- Provider impl ---

func (o *OpenAI) Complete(ctx context.Context, req Request) (*Response, error) {
	if o.APIKey == "" {
		return nil, fmt.Errorf("API key not configured. Set OPENAI_API_KEY environment variable or configure in ~/.metis/config.toml")
	}
	body := toOpenAI(req, o.Model, o.MaxTokens)
	body.Stream = false

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	var or oaiResp
	err = transport.RetryWithBackoff(ctx, 3, 0, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", o.BaseURL+"/chat/completions", bytes.NewReader(buf))
		if err != nil {
			return err
		}
		o.setHeaders(httpReq)
		resp, err := o.httpClient.Do(httpReq)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			httpErr := fmt.Errorf("openai %d: %s", resp.StatusCode, transport.Truncate(string(rb), 500))
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
		return nil, errors.New("openai: empty choices")
	}
	return fromOpenAIChoice(or.Choices[0], or.Usage), nil
}

func (o *OpenAI) Stream(ctx context.Context, req Request) (StreamReader, error) {
	if o.APIKey == "" {
		return nil, fmt.Errorf("API key not configured. Set OPENAI_API_KEY environment variable or configure in ~/.metis/config.toml")
	}
	body := toOpenAI(req, o.Model, o.MaxTokens)
	body.Stream = true

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	var resp *http.Response
	err = transport.RetryWithBackoff(ctx, 3, 0, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", o.BaseURL+"/chat/completions", bytes.NewReader(buf))
		if err != nil {
			return err
		}
		o.setHeaders(httpReq)
		resp, err = o.httpClient.Do(httpReq)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			rb, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			httpErr := fmt.Errorf("openai %d: %s", resp.StatusCode, transport.Truncate(string(rb), 500))
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
	return newOpenAIStream(resp.Body), nil
}

func (o *OpenAI) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+o.APIKey)
}

// --- SSE stream ---

type openAIStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	// per-tool-call accumulator keyed by index
	calls        map[int]*oaiCallAccum
	emittedStart map[int]bool
	// idToIdx maps a tool_call.id to the synthetic index we picked when the
	// upstream provider didn't send one. Some OpenAI-compat servers (Groq,
	// Together) omit the index field on parallel tool calls and the previous
	// "default to 0" code collapsed every call into calls[0].
	idToIdx          map[string]int
	nextSyntheticIdx int
	// Some providers (Gemini's OpenAI-compat layer) bundle multiple logical
	// events into a single SSE chunk (content + finish_reason + usage).
	// We queue events so each Recv() returns exactly one.
	pending []StreamEvent
}

type oaiCallAccum struct {
	ID      string
	Name    string
	JSONBuf strings.Builder
}

func newOpenAIStream(body io.ReadCloser) *openAIStream {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	return &openAIStream{
		body:         body,
		scanner:      sc,
		calls:        make(map[int]*oaiCallAccum),
		emittedStart: make(map[int]bool),
		idToIdx:      make(map[string]int),
	}
}

func (s *openAIStream) Close() error { return s.body.Close() }

// resolveToolCallIdx assigns a stable map key for a streamed tool_call delta.
// The OpenAI spec sends `index` for every chunk, but Groq/Together and other
// compat servers sometimes omit it on parallel calls — we then fall back to
// the call id, and if even that is missing we hand out fresh synthetic
// indices. The previous code defaulted to 0 here, which collapsed every
// indexless tool call into a single accumulator.
func (s *openAIStream) resolveToolCallIdx(tc oaiToolCall) int {
	if tc.Index != nil {
		return *tc.Index
	}
	if tc.ID != "" {
		if known, ok := s.idToIdx[tc.ID]; ok {
			return known
		}
		// Pick a synthetic slot above any real index we've seen so we
		// don't collide with future numbered chunks.
		idx := s.nextSyntheticIdx
		s.nextSyntheticIdx++
		s.idToIdx[tc.ID] = idx
		return idx
	}
	idx := s.nextSyntheticIdx
	s.nextSyntheticIdx++
	return idx
}

func (s *openAIStream) Recv() (StreamEvent, error) {
	if len(s.pending) > 0 {
		ev := s.pending[0]
		s.pending = s.pending[1:]
		// io.EOF still needs to be propagated as the final return.
		if ev.Type == "message_stop" && len(s.pending) == 0 {
			return ev, io.EOF
		}
		return ev, nil
	}
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if dbgOpenAI {
			fmt.Fprintf(os.Stderr, "[oai] raw=%s\n", payload)
		}
		if payload == "[DONE]" {
			// flush any pending tool_use_stop, then enqueue message_stop.
			for idx, c := range s.calls {
				if s.emittedStart[idx] {
					s.pending = append(s.pending, StreamEvent{Type: "tool_use_stop", ToolUseID: c.ID, InputDelta: c.JSONBuf.String()})
				}
				delete(s.calls, idx)
				delete(s.emittedStart, idx)
			}
			s.pending = append(s.pending, StreamEvent{Type: "message_stop"})
			return s.popPending()
		}

		var env struct {
			Choices []struct {
				Delta        oaiMessage `json:"delta"`
				FinishReason string     `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &env); err != nil {
			continue
		}

		// Process choice deltas (content / tool_calls / finish_reason).
		// Each chunk may contain multiple of these — enqueue all, return first.
		if len(env.Choices) > 0 {
			ch := env.Choices[0]
			if ch.Delta.Content != "" {
				s.pending = append(s.pending, StreamEvent{Type: "text_delta", TextDelta: ch.Delta.Content})
			}
			for _, tc := range ch.Delta.ToolCalls {
				idx := s.resolveToolCallIdx(tc)
				c, ok := s.calls[idx]
				if !ok {
					c = &oaiCallAccum{}
					s.calls[idx] = c
				}
				if tc.ID != "" {
					c.ID = tc.ID
				}
				if tc.Function.Name != "" {
					c.Name = tc.Function.Name
					if !s.emittedStart[idx] {
						s.emittedStart[idx] = true
						s.pending = append(s.pending, StreamEvent{Type: "tool_use_start", ToolUseID: c.ID, ToolName: c.Name})
					}
				}
				if tc.Function.Arguments != "" {
					c.JSONBuf.WriteString(tc.Function.Arguments)
					s.pending = append(s.pending, StreamEvent{Type: "tool_input_delta", ToolUseID: c.ID, InputDelta: tc.Function.Arguments})
				}
			}
			if ch.FinishReason != "" {
				for idx, c := range s.calls {
					if s.emittedStart[idx] {
						s.pending = append(s.pending, StreamEvent{Type: "tool_use_stop", ToolUseID: c.ID, InputDelta: c.JSONBuf.String()})
					}
					delete(s.calls, idx)
					delete(s.emittedStart, idx)
				}
				ev := StreamEvent{Type: "message_delta", StopReason: mapOAIStop(ch.FinishReason)}
				if env.Usage != nil {
					ev.InputTokens = env.Usage.PromptTokens
					ev.OutputTokens = env.Usage.CompletionTokens
				}
				s.pending = append(s.pending, ev)
			} else if env.Usage != nil {
				s.pending = append(s.pending, StreamEvent{Type: "message_delta", InputTokens: env.Usage.PromptTokens, OutputTokens: env.Usage.CompletionTokens})
			}
		} else if env.Usage != nil {
			s.pending = append(s.pending, StreamEvent{Type: "message_delta", InputTokens: env.Usage.PromptTokens, OutputTokens: env.Usage.CompletionTokens})
		}

		if len(s.pending) > 0 {
			return s.popPending()
		}
	}
	if err := s.scanner.Err(); err != nil {
		return StreamEvent{Type: "error", Err: err}, err
	}
	return StreamEvent{Type: "message_stop"}, io.EOF
}

func (s *openAIStream) popPending() (StreamEvent, error) {
	ev := s.pending[0]
	s.pending = s.pending[1:]
	if ev.Type == "message_stop" && len(s.pending) == 0 {
		return ev, io.EOF
	}
	return ev, nil
}
