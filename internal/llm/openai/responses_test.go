package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// responsesHarness spins up an httptest SSE server that replays the given
// event lines and records the request body.
type responsesHarness struct {
	server   *httptest.Server
	gotBody  []byte
	gotPath  string
	replay   []string
	failMode string // "", "http_error"
}

func newResponsesHarness(t *testing.T, events []string) *responsesHarness {
	t.Helper()
	h := &responsesHarness{replay: events}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.gotPath = r.URL.Path
		h.gotBody, _ = io.ReadAll(r.Body)
		if h.failMode == "http_error" {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		// Streaming requests (stream:true in body) get the SSE replay;
		// non-streamed Complete calls get the final JSON envelope.
		if strings.Contains(string(h.gotBody), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for _, ev := range h.replay {
				fmt.Fprintf(w, "data: %s\n\n", ev)
				fl.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2}}`))
	}))
	t.Cleanup(h.server.Close)
	return h
}

func newResponsesClient(h *responsesHarness) *Responses {
	return NewResponses("sk-test", h.server.URL, "gpt-5", 4096, 30*time.Second, 0)
}

func drainStream(t *testing.T, r provider.StreamReader) []provider.StreamEvent {
	t.Helper()
	defer r.Close()
	var events []provider.StreamEvent
	for {
		ev, err := r.Recv()
		if err != nil {
			if err == io.EOF {
				return events
			}
			events = append(events, ev) // error event itself
			return events
		}
		events = append(events, ev)
	}
}

func TestResponses_RequestShape(t *testing.T) {
	h := newResponsesHarness(t, []string{`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":2}}}`})
	p := newResponsesClient(h)
	req := provider.Request{
		System: "be terse",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
				{Type: "text", Text: "let me check"},
				{Type: "tool_use", ToolUseID: "call_1", ToolName: "Grep", ToolInput: map[string]any{"pattern": "x"}},
			}},
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", ToolResult: "a.go:1:x"}}},
		},
		Tools: []provider.ToolSpec{{Name: "Grep", Description: "search", InputSchema: map[string]any{"type": "object"}}},
	}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if h.gotPath != "/responses" {
		t.Fatalf("path = %q, want /responses", h.gotPath)
	}
	var body struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Input        []struct {
			Type      string `json:"type"`
			Role      string `json:"role"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Output    string `json:"output"`
		} `json:"input"`
		Tools []struct {
			Type   string         `json:"type"`
			Name   string         `json:"name"`
			Params map[string]any `json:"parameters"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(h.gotBody, &body); err != nil {
		t.Fatalf("request body parse: %v\n%s", err, h.gotBody)
	}
	if body.Instructions != "be terse" {
		t.Fatalf("instructions = %q", body.Instructions)
	}
	// item sequence: user message, assistant message, function_call, function_call_output
	if len(body.Input) != 4 {
		t.Fatalf("input items = %d, want 4: %+v", len(body.Input), body.Input)
	}
	if body.Input[0].Type != "message" || body.Input[0].Role != "user" {
		t.Fatalf("item 0 = %+v", body.Input[0])
	}
	if body.Input[1].Type != "message" || body.Input[1].Role != "assistant" {
		t.Fatalf("item 1 = %+v", body.Input[1])
	}
	if body.Input[2].Type != "function_call" || body.Input[2].CallID != "call_1" || body.Input[2].Name != "Grep" || body.Input[2].Arguments != `{"pattern":"x"}` {
		t.Fatalf("item 2 = %+v", body.Input[2])
	}
	if body.Input[3].Type != "function_call_output" || body.Input[3].CallID != "call_1" || body.Input[3].Output != "a.go:1:x" {
		t.Fatalf("item 3 = %+v", body.Input[3])
	}
	if len(body.Tools) != 1 || body.Tools[0].Type != "function" || body.Tools[0].Name != "Grep" {
		t.Fatalf("tools = %+v", body.Tools)
	}
	if body.Tools[0].Params["type"] != "object" {
		t.Fatalf("tool parameters = %+v", body.Tools[0].Params)
	}
}

func TestResponses_StreamTextAndToolFlow(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.created"}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}`,
		`{"type":"response.output_text.delta","delta":"Hello"}`,
		`{"type":"response.output_text.delta","delta":" world"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_9","name":"Grep","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"call_9","delta":"{\"pat"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"call_9","delta":"tern\":\"x\"}"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_9","name":"Grep","arguments":"{\"pattern\":\"x\"}"}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":20,"input_tokens_details":{"cached_tokens":4}},"output":[{"type":"message"},{"type":"function_call","call_id":"call_9"}]}}`,
	})
	p := newResponsesClient(h)
	r, err := p.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	events := drainStream(t, r)
	got := map[string]int{}
	var stop provider.StreamEvent
	for _, ev := range events {
		got[ev.Type]++
		if ev.Type == "message_delta" {
			stop = ev
		}
		if ev.Type == "tool_use_start" {
			if ev.ToolUseID != "call_9" || ev.ToolName != "Grep" {
				t.Fatalf("tool_use_start = %+v", ev)
			}
		}
		if ev.Type == "tool_use_stop" && ev.InputDelta != `{"pattern":"x"}` {
			t.Fatalf("tool_use_stop args = %q", ev.InputDelta)
		}
	}
	if got["text_delta"] != 2 {
		t.Fatalf("text_delta count = %d, want 2 (%+v)", got["text_delta"], events)
	}
	if got["tool_input_delta"] != 2 {
		t.Fatalf("tool_input_delta count = %d, want 2", got["tool_input_delta"])
	}
	if got["tool_use_start"] != 1 || got["tool_use_stop"] != 1 {
		t.Fatalf("tool flow events missing: %+v", got)
	}
	if stop.StopReason != "tool_use" {
		t.Fatalf("stop reason = %q, want tool_use", stop.StopReason)
	}
	if stop.InputTokens != 6 || stop.OutputTokens != 20 || stop.CacheReadInputTokens != 4 {
		t.Fatalf("usage mapping wrong: %+v", stop)
	}
}

func TestResponses_StreamThinkingAndEndTurn(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.reasoning_summary_text.delta","delta":"step one"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":" step two"}`,
		`{"type":"response.output_text.delta","delta":"answer"}`,
		`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":1}}}`,
	})
	p := newResponsesClient(h)
	r, err := p.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	events := drainStream(t, r)
	think, text, stop := 0, 0, ""
	for _, ev := range events {
		switch ev.Type {
		case "thinking_delta":
			think++
		case "text_delta":
			text++
		case "message_delta":
			stop = ev.StopReason
		}
	}
	if think != 2 || text != 1 || stop != "end_turn" {
		t.Fatalf("think=%d text=%d stop=%q events=%+v", think, text, stop, events)
	}
}

func TestResponses_StreamIncompleteMaxTokens(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
	})
	p := newResponsesClient(h)
	r, err := p.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	events := drainStream(t, r)
	stop := ""
	for _, ev := range events {
		if ev.Type == "message_delta" {
			stop = ev.StopReason
		}
	}
	if stop != "max_tokens" {
		t.Fatalf("stop = %q, want max_tokens", stop)
	}
}

func TestResponses_StreamFailed(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"out of capacity"}}}`,
	})
	p := newResponsesClient(h)
	r, err := p.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	events := drainStream(t, r)
	var gotErr error
	for _, ev := range events {
		if ev.Type == "error" {
			gotErr = ev.Err
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "out of capacity") {
		t.Fatalf("expected failure error, got %v (%+v)", gotErr, events)
	}
}

func TestResponses_HTTPError(t *testing.T) {
	h := newResponsesHarness(t, nil)
	h.failMode = "http_error"
	p := newResponsesClient(h)
	_, err := p.Stream(context.Background(), provider.Request{Stream: true})
	if err == nil || !strings.Contains(err.Error(), "responses 500") {
		t.Fatalf("expected 500 error, got %v", err)
	}
}

func TestResponses_CompleteToolUseAndThinking(t *testing.T) {
	out := `{"status":"completed","usage":{"input_tokens":7,"output_tokens":9,"input_tokens_details":{"cached_tokens":2}},
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"plan first"}]},
			{"type":"message","content":[{"type":"output_text","text":"the answer"}]},
			{"type":"function_call","call_id":"c1","name":"Read","arguments":"{\"path\":\"a.go\"}"}
		]}`
	h := newResponsesHarness(t, nil)
	// replay handler ignores events for Complete; inject via replay anyway
	h.replay = []string{}
	h.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(out))
	})
	p := newResponsesClient(h)
	res, err := p.Complete(context.Background(), provider.Request{})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(res.Content) != 3 {
		t.Fatalf("content blocks = %d, want 3: %+v", len(res.Content), res.Content)
	}
	if res.Content[0].Type != "thinking" || res.Content[0].Text != "plan first" {
		t.Fatalf("block 0 = %+v", res.Content[0])
	}
	if res.Content[1].Type != "text" || res.Content[1].Text != "the answer" {
		t.Fatalf("block 1 = %+v", res.Content[1])
	}
	if res.Content[2].Type != "tool_use" || res.Content[2].ToolName != "Read" || res.Content[2].ToolUseID != "c1" {
		t.Fatalf("block 2 = %+v", res.Content[2])
	}
	if input, ok := res.Content[2].ToolInput["path"].(string); !ok || input != "a.go" {
		t.Fatalf("tool input = %+v", res.Content[2].ToolInput)
	}
	if res.StopReason != "tool_use" {
		t.Fatalf("stop = %q", res.StopReason)
	}
	if res.InputTokens != 5 || res.OutputTokens != 9 || res.CacheReadInputTokens != 2 {
		t.Fatalf("usage = %+v", res)
	}
}
