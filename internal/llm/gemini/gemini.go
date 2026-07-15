// Package gemini implements Google's Generative Language API
// (generativelanguage.googleapis.com/v1beta/models/{model}:streamGenerateContent).
// Distinct from Vertex AI's Anthropic-on-Google offering — this is
// Google's own native protocol with its own message + tool schema.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/sse"
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

var dbgGemini = os.Getenv("METIS_DEBUG_GEMINI") == "1"

// Gemini implements Provider against Google's Generative Language API
// (v1beta). Endpoint pattern matches openclaw/extensions/google:
//
//	POST <base>/v1beta/models/<model>:streamGenerateContent?alt=sse
//	x-goog-api-key: <key>
//
// We always stream — the agent loop only ever calls Stream(), Compactor
// now does too, so Complete() is mostly here to satisfy the Provider
// interface for one-off non-streaming integrations.
type Gemini struct {
	APIKey      string
	BaseURL     string
	Model       string
	MaxTokens   int
	Temperature float64
	// ContextWindow, when > 0, overrides the model-prefix lookup.
	ContextWindow int
	httpClient    *http.Client
}

func New(apiKey, baseURL, model string, maxTokens int, timeout time.Duration, temperature float64) *Gemini {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	if model == "" {
		model = "gemini-2.5-pro"
	}
	if maxTokens == 0 {
		maxTokens = 32000
	}
	return &Gemini{
		APIKey:      apiKey,
		BaseURL:     strings.TrimRight(baseURL, "/"),
		Model:       model,
		MaxTokens:   maxTokens,
		Temperature: temperature,
		httpClient:  transport.NewHTTPClient(timeout, "gemini"),
	}
}

func (g *Gemini) Name() string { return "gemini" }

// ModelID returns the wire-level model id this provider sends.
func (g *Gemini) ModelID() string { return g.Model }

// MaxContextTokens — Gemini 2.5 line ships with a 1M-token window for
// pro / flash; older models cap at 32k or 128k. Pick by model prefix
// so the compactor uses the right denominator.
func (g *Gemini) MaxContextTokens() int {
	if g.ContextWindow > 0 {
		return g.ContextWindow
	}
	switch {
	case strings.HasPrefix(g.Model, "gemini-2.5"), strings.HasPrefix(g.Model, "gemini-2.0"),
		strings.HasPrefix(g.Model, "gemini-1.5"):
		return 1000000
	case strings.HasPrefix(g.Model, "gemini-pro"):
		return 32000
	default:
		return 1000000
	}
}

// --- request shapes ---

type gemPart struct {
	Text             string           `json:"text,omitempty"`
	FunctionCall     *gemFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *gemFunctionResp `json:"functionResponse,omitempty"`
	// ThoughtSignature is Gemini-3.5+ thinking-model artifact: the
	// model emits this opaque blob on parts that carry a function
	// call; clients MUST echo it back on the corresponding history
	// entry or Gemini rejects the next turn with
	//   "Function call is missing a thought_signature"
	// (gemini-3.5-flash 2026-05 enforcement). Captured in
	// fromGeminiChunk, replayed via toGemini.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type gemFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type gemFunctionResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type gemContent struct {
	Role  string    `json:"role,omitempty"` // "user" | "model" — no "system" or "tool"
	Parts []gemPart `json:"parts"`
}

type gemTool struct {
	FunctionDeclarations []gemFunctionDecl `json:"functionDeclarations"`
}

type gemFunctionDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type gemGenConfig struct {
	Temperature     *float64        `json:"temperature,omitempty"`
	MaxOutputTokens int             `json:"maxOutputTokens,omitempty"`
	StopSequences   []string        `json:"stopSequences,omitempty"`
	ThinkingConfig  *gemThinkingCfg `json:"thinkingConfig,omitempty"`
}

// gemThinkingCfg controls Gemini's thinking mode (gemini-2.5-pro,
// gemini-3.5-flash, etc). thinkingBudget=0 disables thinking entirely,
// which avoids the thought_signature round-trip requirement that
// gemini-3.5-flash enforces on function_call parts. metis's ContentBlock
// doesn't yet carry the signature, so without this setting we'd see:
//
//	400 Function call is missing a thought_signature in functionCall
//	    parts. This is required for tools to work correctly.
//
// Future: capture + echo thought_signature → re-enable thinking via
// METIS_GEMINI_THINKING=low/high.
type gemThinkingCfg struct {
	// ThinkingBudget=0 disables; -1 = default (model-decided).
	// Positive values cap thinking tokens.
	ThinkingBudget *int `json:"thinkingBudget,omitempty"`
}

type gemReq struct {
	Contents          []gemContent  `json:"contents"`
	SystemInstruction *gemContent   `json:"systemInstruction,omitempty"`
	Tools             []gemTool     `json:"tools,omitempty"`
	GenerationConfig  *gemGenConfig `json:"generationConfig,omitempty"`
}

// --- conversion: internal Request → Gemini gemReq ---

func toGemini(req Request, maxTokens int) gemReq {
	mt := maxTokens
	if req.MaxTokens > 0 {
		mt = req.MaxTokens
	}
	out := gemReq{
		GenerationConfig: &gemGenConfig{
			MaxOutputTokens: mt,
			StopSequences:   req.StopSequences,
			// 2026-05-23: thinking off by default (budget=0).
			// gemini-3.5-flash requires thought_signature echoed on
			// every function_call once thinking is on; metis's
			// ContentBlock doesn't carry the signature yet, so we
			// disable thinking to avoid the 400 "missing
			// thought_signature" error. Override via env later when
			// we wire signature round-trip.
			ThinkingConfig: geminiThinkingConfig(),
		},
	}
	if req.Temperature > 0 {
		t := req.Temperature
		out.GenerationConfig.Temperature = &t
	}
	if req.System != "" {
		out.SystemInstruction = &gemContent{
			Parts: []gemPart{{Text: req.System}},
		}
	}
	// 2026-05-23: Gemini requires functionResponse.name to be
	// non-empty (mirrors the function_call.name that triggered it).
	// metis's ContentBlock for tool_result only carries ToolUseID,
	// not ToolName, so we build a map from tool_use_id → tool_name
	// by pre-scanning the request's assistant messages.
	toolUseIDToName := map[string]string{}
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if c.Type == "tool_use" && c.ToolUseID != "" && c.ToolName != "" {
				toolUseIDToName[c.ToolUseID] = c.ToolName
			}
		}
	}

	for _, m := range req.Messages {
		var role string
		switch m.Role {
		case RoleUser:
			role = "user"
		case RoleAssistant:
			role = "model"
		case RoleSystem:
			// Gemini has only user/model; system folds into systemInstruction
			// so a stray system message in the middle of the history (e.g. a
			// compactor boundary) becomes a user-tagged note.
			role = "user"
		default:
			role = "user"
		}
		var parts []gemPart
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				if c.Text != "" {
					parts = append(parts, gemPart{Text: c.Text})
				}
			case "tool_use":
				gp := gemPart{FunctionCall: &gemFunctionCall{
					Name: c.ToolName, Args: c.ToolInput,
				}}
				// Echo thoughtSignature back if we captured one on
				// the originating response — gemini-3.5+ requires it
				// on every function_call replayed in history.
				if c.ProviderHint != nil {
					if sig := c.ProviderHint["gemini.thought_signature"]; sig != "" {
						gp.ThoughtSignature = sig
					}
				}
				parts = append(parts, gp)
			case "tool_result":
				// Gemini's functionResponse must wrap the tool output in
				// an object — strings are not allowed at the top of
				// `response`. Wrap raw text under `content` so the model
				// can still read it as JSON.
				name := c.ToolName
				if name == "" {
					name = toolUseIDToName[c.ToolUseID]
				}
				if name == "" {
					// Fallback so Gemini doesn't reject "Name cannot
					// be empty"; happens when an orphan tool_result
					// has no matching tool_use in the visible window
					// (post-compact, partial replay). "unknown" loses
					// the name signal but keeps the request valid.
					name = "unknown"
				}
				parts = append(parts, gemPart{FunctionResponse: &gemFunctionResp{
					Name:     name,
					Response: map[string]any{"content": c.ToolResult, "is_error": c.IsError},
				}})
			}
		}
		if len(parts) == 0 {
			continue
		}
		out.Contents = append(out.Contents, gemContent{Role: role, Parts: parts})
	}
	for _, t := range req.Tools {
		// Gemini batches all function declarations under a single tools
		// entry. Schema sanitized first — Gemini's parser rejects JSON
		// Schema fields that OpenAI/Anthropic accept (additionalProperties,
		// $schema, etc.). Repro 2026-05-23 with gemini-3.5-flash:
		//   "Unknown name \"additionalProperties\" at
		//    'tools[40].function_declarations[0].parameters': Cannot
		//    find field."
		out.Tools = append(out.Tools, gemTool{
			FunctionDeclarations: []gemFunctionDecl{{
				Name: t.Name, Description: t.Description,
				Parameters: sanitizeSchemaForGemini(t.InputSchema),
			}},
		})
	}
	return out
}

// sanitizeSchemaForGemini recursively strips JSON Schema fields Gemini
// rejects. The whitelist approach (keep only known-good fields) is
// safer than blacklist because Gemini's API spec evolves and silently
// 400s on new unknown fields.
//
// Kept fields per Google's API spec
// (https://ai.google.dev/api/caching#Schema):
//
//	type, format, description, nullable, enum, properties, required,
//	items, minimum, maximum, minLength, maxLength, pattern, default
//
// Stripped: additionalProperties, $schema, $defs, $ref, oneOf, anyOf,
// allOf, not, examples, title, const, ...
func sanitizeSchemaForGemini(s map[string]any) map[string]any {
	if s == nil {
		return nil
	}
	allowed := map[string]bool{
		"type": true, "format": true, "description": true, "nullable": true,
		"enum": true, "properties": true, "required": true, "items": true,
		"minimum": true, "maximum": true, "minLength": true, "maxLength": true,
		"pattern": true, "default": true,
	}
	out := make(map[string]any, len(s))
	for k, v := range s {
		if !allowed[k] {
			continue
		}
		switch k {
		case "properties":
			if props, ok := v.(map[string]any); ok {
				cleanProps := make(map[string]any, len(props))
				for name, schema := range props {
					if sub, ok := schema.(map[string]any); ok {
						cleanProps[name] = sanitizeSchemaForGemini(sub)
					} else {
						cleanProps[name] = schema
					}
				}
				out[k] = cleanProps
			} else {
				out[k] = v
			}
		case "items":
			if sub, ok := v.(map[string]any); ok {
				out[k] = sanitizeSchemaForGemini(sub)
			} else {
				out[k] = v
			}
		default:
			out[k] = v
		}
	}
	return out
}

// --- response shapes (for both streaming chunks and Complete) ---

type gemRespChunk struct {
	Candidates []struct {
		Content      gemContent `json:"content"`
		FinishReason string     `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
}

// --- Provider impl ---

func (g *Gemini) endpoint(streaming bool) string {
	op := "generateContent"
	if streaming {
		op = "streamGenerateContent"
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:%s", g.BaseURL, g.Model, op)
	if streaming {
		url += "?alt=sse"
	}
	return url
}

func (g *Gemini) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-goog-api-key", g.APIKey)
}

func (g *Gemini) Complete(ctx context.Context, req Request) (*Response, error) {
	if g.APIKey == "" {
		return nil, fmt.Errorf("API key not configured. Set GEMINI_API_KEY (or GOOGLE_API_KEY) environment variable or configure in ~/.metis/config.toml")
	}
	body := toGemini(req, g.MaxTokens)
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var chunk gemRespChunk
	err = transport.RetryWithBackoff(ctx, 3, 0, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", g.endpoint(false), bytes.NewReader(buf))
		if err != nil {
			return err
		}
		g.setHeaders(httpReq)
		resp, err := g.httpClient.Do(httpReq)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			httpErr := fmt.Errorf("gemini %d: %s", resp.StatusCode, transport.Truncate(string(rb), 500))
			if transport.IsRetryableStatus(resp.StatusCode) {
				return &transport.RetryableError{Err: httpErr, After: transport.ParseRetryAfter(resp)}
			}
			return httpErr
		}
		return json.Unmarshal(rb, &chunk)
	})
	if err != nil {
		return nil, err
	}
	return fromGeminiChunk(chunk), nil
}

func (g *Gemini) Stream(ctx context.Context, req Request) (StreamReader, error) {
	if g.APIKey == "" {
		return nil, fmt.Errorf("API key not configured. Set GEMINI_API_KEY (or GOOGLE_API_KEY) environment variable or configure in ~/.metis/config.toml")
	}
	body := toGemini(req, g.MaxTokens)
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if dbgGemini {
		fmt.Fprintf(os.Stderr, "[gemini] POST %s\n%s\n", g.endpoint(true), buf)
	}
	var resp *http.Response
	err = transport.RetryWithBackoff(ctx, 3, 0, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", g.endpoint(true), bytes.NewReader(buf))
		if err != nil {
			return err
		}
		g.setHeaders(httpReq)
		httpReq.Header.Set("Accept", "text/event-stream")
		resp, err = g.httpClient.Do(httpReq)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			rb, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			httpErr := fmt.Errorf("gemini %d: %s", resp.StatusCode, transport.Truncate(string(rb), 500))
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
	return newGeminiStream(resp.Body), nil
}

// --- response → internal Response (non-streaming) ---

func fromGeminiChunk(chunk gemRespChunk) *Response {
	r := &Response{
		InputTokens:  chunk.UsageMetadata.PromptTokenCount,
		OutputTokens: chunk.UsageMetadata.CandidatesTokenCount,
	}
	if len(chunk.Candidates) == 0 {
		return r
	}
	cand := chunk.Candidates[0]
	r.StopReason = mapGeminiStopReason(cand.FinishReason)
	for _, p := range cand.Content.Parts {
		if p.Text != "" {
			r.Content = append(r.Content, ContentBlock{Type: "text", Text: p.Text})
		}
		if p.FunctionCall != nil {
			block := ContentBlock{
				Type:      "tool_use",
				ToolUseID: gemToolID(p.FunctionCall.Name),
				ToolName:  p.FunctionCall.Name,
				ToolInput: p.FunctionCall.Args,
			}
			// gemini-3.5+ thinking models attach a thoughtSignature
			// to function_call parts. Stash on ProviderHint so the
			// next turn's toGemini() can echo it back — Gemini
			// rejects the follow-up turn otherwise.
			if p.ThoughtSignature != "" {
				block.ProviderHint = map[string]string{"gemini.thought_signature": p.ThoughtSignature}
			}
			r.Content = append(r.Content, block)
		}
	}
	return r
}

// gemToolID synthesizes a stable id since Gemini's functionCall has no
// id field — the agent loop's tool result lookup keys off ToolUseID, so
// we hash the name + a counter. Single-call turns get name-as-id; the
// streaming path overwrites this with a per-call counter.
func gemToolID(name string) string {
	return "gem_" + name
}

func mapGeminiStopReason(r string) string {
	switch r {
	case "STOP":
		return "end_turn"
	case "MAX_TOKENS":
		return "max_tokens"
	case "TOOL_USE", "FUNCTION_CALL":
		return "tool_use"
	case "STOP_SEQUENCE":
		return "stop_sequence"
	}
	if r == "" {
		return ""
	}
	return strings.ToLower(r)
}

// --- streaming SSE reader ---

type geminiStream struct {
	body io.ReadCloser
	sse  *sse.Reader
	// idCounter generates synthetic tool_use ids since Gemini's
	// functionCall has no id field. Each tool_use_start emits a fresh
	// "gem_<n>" so multiple parallel calls in one chunk don't collide.
	idCounter int
	// pending events queued from a single SSE frame so each Recv()
	// returns exactly one (Gemini bundles text + functionCall + finish
	// in a single chunk).
	pending []StreamEvent
	// done flips true after we emit message_stop so subsequent Recv
	// calls return io.EOF instead of re-parsing.
	done bool
}

func newGeminiStream(body io.ReadCloser) *geminiStream {
	return &geminiStream{body: body, sse: sse.NewReader(body)}
}

func (s *geminiStream) Close() error { return s.body.Close() }

func (s *geminiStream) Recv() (StreamEvent, error) {
	if len(s.pending) > 0 {
		ev := s.pending[0]
		s.pending = s.pending[1:]
		return ev, nil
	}
	if s.done {
		return StreamEvent{}, io.EOF
	}
	for {
		frame, err := s.sse.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Clean stream end without explicit message_stop —
				// emit one and flip `done` so further Recv returns EOF.
				s.done = true
				return StreamEvent{Type: "message_stop"}, nil
			}
			return StreamEvent{}, err
		}
		// sse.Reader already trims spec-mandated single leading space;
		// some Gemini gateways add extra spaces around the JSON, so
		// be generous when checking for empty.
		payload := strings.TrimSpace(frame.Data)
		if payload == "" {
			continue
		}
		var chunk gemRespChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // skip malformed line, keep reading
		}
		s.queueChunk(chunk)
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
	}
}

// queueChunk fans one Gemini SSE chunk out into the linear StreamEvent
// sequence the agent loop consumes (text_delta, tool_use_start,
// tool_input_delta, tool_use_stop, message_delta, message_stop).
func (s *geminiStream) queueChunk(chunk gemRespChunk) {
	if len(chunk.Candidates) == 0 {
		// Some chunks carry only usageMetadata — surface tokens then
		// move on without producing any content events.
		if chunk.UsageMetadata.PromptTokenCount > 0 || chunk.UsageMetadata.CandidatesTokenCount > 0 {
			s.pending = append(s.pending, StreamEvent{
				Type:         "message_delta",
				InputTokens:  chunk.UsageMetadata.PromptTokenCount,
				OutputTokens: chunk.UsageMetadata.CandidatesTokenCount,
			})
		}
		return
	}
	cand := chunk.Candidates[0]
	for _, p := range cand.Content.Parts {
		if p.Text != "" {
			s.pending = append(s.pending, StreamEvent{Type: "text_delta", TextDelta: p.Text})
		}
		if p.FunctionCall != nil {
			s.idCounter++
			id := fmt.Sprintf("gem_%d", s.idCounter)
			startEv := StreamEvent{Type: "tool_use_start", ToolUseID: id, ToolName: p.FunctionCall.Name}
			// gemini-3.5+ thinking models attach thoughtSignature to
			// the same part as functionCall. Capture it onto the
			// tool_use_start event so the loop's consumeStream can
			// stash it on the ContentBlock that gets persisted into
			// message history — next turn's toGemini() echoes it
			// back, satisfying Gemini's "Function call is missing a
			// thought_signature" enforcement.
			if p.ThoughtSignature != "" {
				startEv.ProviderHint = map[string]string{"gemini.thought_signature": p.ThoughtSignature}
			}
			s.pending = append(s.pending, startEv)
			if len(p.FunctionCall.Args) > 0 {
				args, _ := json.Marshal(p.FunctionCall.Args)
				s.pending = append(s.pending,
					StreamEvent{Type: "tool_input_delta", ToolUseID: id, InputDelta: string(args)},
				)
			}
			s.pending = append(s.pending,
				StreamEvent{Type: "tool_use_stop", ToolUseID: id},
			)
		}
	}
	if cand.FinishReason != "" {
		// Gemini's final chunk reports BOTH promptTokenCount and
		// candidatesTokenCount under usageMetadata. The previous code
		// only forwarded OutputTokens, which left consumeStream's
		// usage.in stuck at 0 whenever Gemini didn't emit a separate
		// usage-only chunk — the bottom-right counter then under-
		// reported real consumption by the input share (often 90%+
		// of the total in tool-heavy turns).
		s.pending = append(s.pending, StreamEvent{
			Type:         "message_delta",
			StopReason:   mapGeminiStopReason(cand.FinishReason),
			InputTokens:  chunk.UsageMetadata.PromptTokenCount,
			OutputTokens: chunk.UsageMetadata.CandidatesTokenCount,
		})
		s.pending = append(s.pending, StreamEvent{Type: "message_stop"})
		s.done = true
	}
}

// geminiThinkingConfig builds the thinkingConfig for Gemini requests.
// Default disables thinking (budget=0) so gemini-3.5-flash doesn't
// require thought_signature echo. Set METIS_GEMINI_THINKING_BUDGET to
// an integer (0 to disable, -1 for model-decided, positive value to
// cap thinking tokens) to override.
func geminiThinkingConfig() *gemThinkingCfg {
	budget := 0
	if v := strings.TrimSpace(envLookup("METIS_GEMINI_THINKING_BUDGET")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			budget = n
		}
	}
	return &gemThinkingCfg{ThinkingBudget: &budget}
}

// envLookup is a tiny stdlib wrapper so the body above stays compact
// and so the test rig can inject without monkey-patching os.Getenv.
var envLookup = func(k string) string { return os.Getenv(k) }
