package openai

import (
	"context"
	"encoding/json"
	"errors"
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
	server       *httptest.Server
	gotBody      []byte
	gotPath      string
	replay       []string
	completeBody string
	failMode     string // "", "http_error"
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
		body := h.completeBody
		if body == "" {
			body = `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2}}`
		}
		_, _ = w.Write([]byte(body))
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

func TestResponses_StreamForcesStreamTrue(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
	})
	p := newResponsesClient(h)

	stream, err := p.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var body map[string]any
	if err := json.Unmarshal(h.gotBody, &body); err != nil {
		t.Fatalf("request body parse: %v\n%s", err, h.gotBody)
	}
	if got, ok := body["stream"].(bool); !ok || !got {
		t.Fatalf("stream = %#v, want true; request=%s", body["stream"], h.gotBody)
	}
}

func TestResponses_StreamAcceptsResponseDone(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.output_text.delta","delta":"done text"}`,
		`{"type":"response.done","response":{"id":"resp_done","status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}`,
	})
	p := newResponsesClient(h)
	stream, err := p.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	events := drainStream(t, stream)
	var text, stop string
	for _, event := range events {
		if event.Type == "text_delta" {
			text += event.TextDelta
		}
		if event.Type == "message_delta" {
			stop = event.StopReason
		}
	}
	if text != "done text" || stop != "end_turn" {
		t.Fatalf("events = %+v", events)
	}
}

func TestResponses_StreamRejectsMalformedJSONBeforeTerminal(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.output_text.delta","delta":"partial"}`,
		`{"type":"response.output_text.delta","delta":`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	})
	p := newResponsesClient(h)
	stream, err := p.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first delta: %v", err)
	}
	event, err := stream.Recv()
	if err == nil || event.Type != "error" || !strings.Contains(err.Error(), "malformed JSON") {
		t.Fatalf("malformed frame result = event=%+v err=%v", event, err)
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

func TestResponses_RequestImageUsesOfficialInputImageShape(t *testing.T) {
	h := newResponsesHarness(t, nil)
	p := newResponsesClient(h)
	req := provider.Request{Messages: []provider.Message{{
		Role: provider.RoleUser,
		Content: []provider.ContentBlock{
			{Type: "text", Text: "describe this"},
			{Type: "image", MediaType: "image/png", Data: "aGVsbG8="},
		},
	}}}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}
	var body struct {
		Input []struct {
			Content []map[string]any `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(h.gotBody, &body); err != nil {
		t.Fatalf("request body parse: %v\n%s", err, h.gotBody)
	}
	if len(body.Input) != 1 || len(body.Input[0].Content) != 2 {
		t.Fatalf("input content = %#v", body.Input)
	}
	image := body.Input[0].Content[1]
	if image["type"] != "input_image" || image["image_url"] != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image part = %#v, want official input_image/image_url shape", image)
	}
	if _, exists := image["text"]; exists {
		t.Fatalf("image part leaked text field: %#v", image)
	}
}

func TestResponses_ProviderStateUsesPreviousResponseIDAndOnlySendsTail(t *testing.T) {
	h := newResponsesHarness(t, nil)
	p := newResponsesClient(h)
	p.StateMode = ResponsesStateProvider
	stateHint := map[string]string{
		responsesHintResponseID: "resp_previous",
		responsesHintStateKey:   p.stateKey(),
	}
	req := provider.Request{
		System: "current instructions",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "old user"}}},
			{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
				{Type: "text", Text: "old assistant"},
				{Type: "provider_state", ProviderHint: stateHint},
			}},
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "new user"}}},
		},
	}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}
	var body struct {
		Store              bool   `json:"store"`
		PreviousResponseID string `json:"previous_response_id"`
		Instructions       string `json:"instructions"`
		Input              []struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(h.gotBody, &body); err != nil {
		t.Fatalf("request body parse: %v\n%s", err, h.gotBody)
	}
	if !body.Store || body.PreviousResponseID != "resp_previous" {
		t.Fatalf("state fields = store:%v previous:%q", body.Store, body.PreviousResponseID)
	}
	if body.Instructions != "current instructions" {
		t.Fatalf("instructions = %q", body.Instructions)
	}
	if len(body.Input) != 1 || body.Input[0].Role != "user" || len(body.Input[0].Content) != 1 || body.Input[0].Content[0].Text != "new user" {
		t.Fatalf("input = %#v, want only new tail", body.Input)
	}
}

func TestResponses_ProviderStateKeepsVolatileSectionsOutOfStoredInput(t *testing.T) {
	p := newResponsesClient(newResponsesHarness(t, nil))
	p.StateMode = ResponsesStateProvider
	stateHint := map[string]string{
		responsesHintResponseID: "resp_previous",
		responsesHintStateKey:   p.stateKey(),
	}
	messages := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "provider_state", ProviderHint: stateHint}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "new user"}}},
	}

	withRuntime, err := p.buildResponsesRequest(provider.Request{
		SystemSections: []provider.SystemSection{
			{Name: "base", Body: "stable", Cache: true},
			{Name: "runtime", Body: "cwd=/current", Volatile: true},
		},
		Messages: messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	if withRuntime.PreviousResponseID != "resp_previous" {
		t.Fatalf("previous_response_id = %q", withRuntime.PreviousResponseID)
	}
	if withRuntime.Instructions != "stable\n\ncwd=/current" {
		t.Fatalf("instructions = %q, want current stable and volatile sections", withRuntime.Instructions)
	}
	if len(withRuntime.Input) != 1 || withRuntime.Input[0].Role != "user" {
		t.Fatalf("provider-managed input = %#v, want only the new user tail", withRuntime.Input)
	}

	withoutRuntime, err := p.buildResponsesRequest(provider.Request{
		SystemSections: []provider.SystemSection{{Name: "base", Body: "stable", Cache: true}},
		Messages:       messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	if withoutRuntime.PreviousResponseID != "resp_previous" {
		t.Fatalf("next previous_response_id = %q", withoutRuntime.PreviousResponseID)
	}
	if withoutRuntime.Instructions != "stable" || strings.Contains(withoutRuntime.Instructions, "cwd=/current") {
		t.Fatalf("next instructions retained old volatile state: %q", withoutRuntime.Instructions)
	}
	if len(withoutRuntime.Input) != 1 || withoutRuntime.Input[0].Role != "user" {
		t.Fatalf("next provider-managed input = %#v, want only the new user tail", withoutRuntime.Input)
	}
}

func TestResponses_StreamRecoversFromMissingPreviousResponseID(t *testing.T) {
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"previous_response_id 'expired' not found"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_recovered\",\"status\":\"completed\",\"usage\":{\"input_tokens\":8,\"output_tokens\":2}}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	p := NewResponses("sk-test", server.URL, "gpt-5", 4096, 30*time.Second, 0)
	p.StateMode = ResponsesStateProvider
	req := provider.Request{Stream: true, Messages: stateRecoveryHistory(p)}
	stream, err := p.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drainStream(t, stream)
	if len(bodies) != 2 {
		t.Fatalf("request count = %d, want initial state request plus one full-history recovery", len(bodies))
	}
	assertStateRecoveryBodies(t, bodies)
	foundState := false
	for _, event := range events {
		if event.Type == "provider_state" && event.ProviderHint[responsesHintResponseID] == "resp_recovered" {
			foundState = true
		}
	}
	if !foundState {
		t.Fatalf("recovered stream did not emit fresh provider state: %#v", events)
	}
}

func TestResponses_CompleteRecoversFromMissingPreviousResponseID(t *testing.T) {
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"previous_response_id 'expired' not found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_recovered","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":8,"output_tokens":2}}`)
	}))
	defer server.Close()

	p := NewResponses("sk-test", server.URL, "gpt-5", 4096, 30*time.Second, 0)
	p.StateMode = ResponsesStateProvider
	response, err := p.Complete(context.Background(), provider.Request{Messages: stateRecoveryHistory(p)})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("request count = %d, want initial state request plus one full-history recovery", len(bodies))
	}
	assertStateRecoveryBodies(t, bodies)
	foundState := false
	for _, block := range response.Content {
		if block.Type == "provider_state" && block.ProviderHint[responsesHintResponseID] == "resp_recovered" {
			foundState = true
		}
	}
	if !foundState {
		t.Fatalf("recovered completion did not return fresh provider state: %#v", response.Content)
	}
}

func stateRecoveryHistory(p *Responses) []provider.Message {
	return []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "old user"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
			{Type: "text", Text: "old assistant"},
			{Type: "provider_state", ProviderHint: map[string]string{
				responsesHintResponseID: "resp_expired",
				responsesHintStateKey:   p.stateKey(),
			}},
		}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "new user"}}},
	}
}

func assertStateRecoveryBodies(t *testing.T, bodies [][]byte) {
	t.Helper()
	type requestBody struct {
		Store              bool                 `json:"store"`
		PreviousResponseID string               `json:"previous_response_id"`
		Input              []responsesInputItem `json:"input"`
	}
	var first, recovered requestBody
	if err := json.Unmarshal(bodies[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bodies[1], &recovered); err != nil {
		t.Fatal(err)
	}
	if first.PreviousResponseID != "resp_expired" || len(first.Input) != 1 {
		t.Fatalf("initial state request = %#v", first)
	}
	if recovered.PreviousResponseID != "" || !recovered.Store || len(recovered.Input) != 3 {
		t.Fatalf("recovery request = %#v, want full history without previous_response_id and store=true", recovered)
	}
}

func TestResponses_StateRecoveryKeepsVolatileSectionsOutOfStoredInput(t *testing.T) {
	p := newResponsesClient(newResponsesHarness(t, nil))
	p.StateMode = ResponsesStateProvider
	body, err := p.buildStateRecoveryRequest(provider.Request{
		SystemSections: []provider.SystemSection{
			{Name: "base", Body: "stable", Cache: true},
			{Name: "runtime", Body: "ephemeral", Volatile: true},
		},
		Messages: stateRecoveryHistory(p),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !body.Store || body.PreviousResponseID != "" {
		t.Fatalf("recovery state fields = store:%v previous:%q", body.Store, body.PreviousResponseID)
	}
	if body.Instructions != "stable\n\nephemeral" {
		t.Fatalf("recovery instructions = %q", body.Instructions)
	}
	for _, item := range body.Input {
		if item.Role == "developer" {
			t.Fatalf("volatile section leaked into stored recovery input: %#v", body.Input)
		}
	}
}

func TestResponses_LocalStateRequestsAndReplaysEncryptedReasoning(t *testing.T) {
	h := newResponsesHarness(t, nil)
	p := newResponsesClient(h)
	p.StateMode = ResponsesStateLocal
	p.Capabilities.EncryptedReasoning = true
	req := provider.Request{Messages: []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{
			Type: "redacted_thinking",
			Data: "encrypted-payload",
			ProviderHint: map[string]string{
				responsesHintItemID: "rs_123",
			},
		}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "continue"}}},
	}}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}
	var body struct {
		Store   bool     `json:"store"`
		Include []string `json:"include"`
		Input   []struct {
			Type             string `json:"type"`
			ID               string `json:"id"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(h.gotBody, &body); err != nil {
		t.Fatalf("request body parse: %v\n%s", err, h.gotBody)
	}
	if body.Store {
		t.Fatal("local state must keep store=false")
	}
	if len(body.Include) != 1 || body.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", body.Include)
	}
	if len(body.Input) < 1 || body.Input[0].Type != "reasoning" || body.Input[0].ID != "rs_123" || body.Input[0].EncryptedContent != "encrypted-payload" {
		t.Fatalf("reasoning input = %#v", body.Input)
	}
}

func TestResponses_AutoPromptCacheKeyIgnoresVolatileSystemSection(t *testing.T) {
	p := NewResponses("sk-test", "https://api.openai.com/v1", "gpt-5", 4096, 30*time.Second, 0)
	reqA := provider.Request{
		SystemSections: []provider.SystemSection{
			{Name: "base", Body: "stable", Cache: true},
			{Name: "runtime", Body: "cwd=/a", Volatile: true},
		},
		Tools: []provider.ToolSpec{{Name: "Read", InputSchema: map[string]any{"type": "object"}}},
	}
	reqB := reqA
	reqB.SystemSections = append([]provider.SystemSection(nil), reqA.SystemSections...)
	reqB.SystemSections[1].Body = "cwd=/b"
	bodyA, err := p.buildResponsesRequest(reqA)
	if err != nil {
		t.Fatal(err)
	}
	bodyB, err := p.buildResponsesRequest(reqB)
	if err != nil {
		t.Fatal(err)
	}
	if bodyA.PromptCacheKey == "" || bodyA.PromptCacheKey != bodyB.PromptCacheKey {
		t.Fatalf("cache keys = %q / %q, want same non-empty key", bodyA.PromptCacheKey, bodyB.PromptCacheKey)
	}
	if bodyA.Instructions != "stable" || len(bodyA.Input) == 0 || bodyA.Input[0].Role != "developer" {
		t.Fatalf("stable/dynamic split = instructions:%q input:%#v", bodyA.Instructions, bodyA.Input)
	}
}

func TestResponses_VolatileSystemSectionPreservesConversationPrefix(t *testing.T) {
	p := NewResponses("sk-test", "https://api.openai.com/v1", "gpt-5", 4096, 30*time.Second, 0)
	body, err := p.buildResponsesRequest(provider.Request{
		SystemSections: []provider.SystemSection{
			{Name: "base", Body: "stable", Cache: true},
			{Name: "auto-retrieve", Body: "query-specific", Volatile: true},
		},
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "old user"}}},
			{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "text", Text: "old assistant"}}},
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "latest user"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 4 {
		t.Fatalf("input = %#v", body.Input)
	}
	roles := []string{body.Input[0].Role, body.Input[1].Role, body.Input[2].Role, body.Input[3].Role}
	want := []string{"user", "assistant", "user", "developer"}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %#v, want %#v", roles, want)
		}
	}
	if got := body.Input[3].Content[0].Text; got != "query-specific" {
		t.Fatalf("volatile content = %q", got)
	}

	toolBody, err := p.buildResponsesRequest(provider.Request{
		SystemSections: []provider.SystemSection{{Name: "runtime", Body: "dynamic", Volatile: true}},
		Messages: []provider.Message{
			{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{
				Type: "tool_use", ToolUseID: "call_1", ToolName: "Read", ToolInput: map[string]any{"path": "a.go"},
			}}},
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{
				Type: "tool_result", ToolUseID: "call_1", ToolResult: "ok",
			}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(toolBody.Input) != 3 || toolBody.Input[0].Type != "function_call" ||
		toolBody.Input[1].Type != "function_call_output" || toolBody.Input[2].Role != "developer" {
		t.Fatalf("volatile section broke function-call/output adjacency: %#v", toolBody.Input)
	}
}

func TestResponses_OpenRouterAutoStateRemainsLocal(t *testing.T) {
	p := NewResponses("sk-test", "https://openrouter.ai/api/v1", "openai/gpt-5", 4096, 30*time.Second, 0)
	p.StateMode = ResponsesStateAuto
	if p.Capabilities.StatefulResponses {
		t.Fatal("OpenRouter create Responses schema only accepts store=false; auto state must remain local")
	}
	body, err := p.buildResponsesRequest(provider.Request{Messages: []provider.Message{
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "old"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "provider_state", ProviderHint: map[string]string{
			responsesHintResponseID: "resp_old",
			responsesHintStateKey:   p.stateKey(),
		}}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "new"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if body.Store || body.PreviousResponseID != "" || len(body.Input) != 2 {
		t.Fatalf("OpenRouter auto request = store:%v previous:%q input:%#v", body.Store, body.PreviousResponseID, body.Input)
	}
}

func TestResponses_MissingPreviousResponseVariants(t *testing.T) {
	for _, raw := range []string{
		`{"error":{"code":"not_found","message":"previous_response_id 'expired' not found"}}`,
		`{"error":{"code":"previous_response_not_found","message":"Previous response with id 'expired' not found."}}`,
		`{"code":"previous_response_not_found","message":"Previous response with id 'expired' not found."}`,
	} {
		if !isMissingPreviousResponse(raw) {
			t.Errorf("did not recognize missing previous response: %s", raw)
		}
	}
	if isMissingPreviousResponse(`{"error":{"code":"not_found","message":"model not found"}}`) {
		t.Fatal("unrelated not_found must not trigger a full-history retry")
	}
}

func TestResponses_KnownTextOnlyModelWinsOverEndpointImageSupport(t *testing.T) {
	p := NewResponses("sk-test", "https://api.openai.com/v1", "gpt-3.5-turbo", 4096, 30*time.Second, 0)
	if got := p.VisionCapability(); got != provider.VisionUnsupported {
		t.Fatalf("VisionCapability = %v, want explicit model-level unsupported", got)
	}
}

func TestResponses_CompatibleProfileDoesNotReplayEncryptedReasoning(t *testing.T) {
	p := NewResponses("sk-test", "https://gateway.example/v1", "model", 4096, 30*time.Second, 0)
	if err := p.ConfigureCapabilityProfile("compatible"); err != nil {
		t.Fatal(err)
	}
	body, err := p.buildResponsesRequest(provider.Request{Messages: []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{
			Type: "redacted_thinking", Data: "opaque", ProviderHint: map[string]string{responsesHintItemID: "rs_1"},
		}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "continue"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range body.Input {
		if item.Type == "reasoning" {
			t.Fatalf("compatible profile replayed unsupported encrypted reasoning: %#v", item)
		}
	}
}

func TestResponses_NativeStructuredOutputAndHostedWebSearch(t *testing.T) {
	h := newResponsesHarness(t, nil)
	p := newResponsesClient(h)
	p.Capabilities.StructuredOutputs = true
	p.Capabilities.HostedTools = true
	p.HostedTools = []string{"web_search"}
	req := provider.Request{ResponseFormat: &provider.ResponseFormat{
		Name:        "answer",
		Description: "machine-readable result",
		JSONSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"answer": map[string]any{"type": "string"},
			},
			"required":             []any{"answer"},
			"additionalProperties": false,
		},
		Strict: true,
	}}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Text struct {
			Format struct {
				Type   string         `json:"type"`
				Name   string         `json:"name"`
				Schema map[string]any `json:"schema"`
				Strict bool           `json:"strict"`
			} `json:"format"`
		} `json:"text"`
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(h.gotBody, &body); err != nil {
		t.Fatal(err)
	}
	if body.Text.Format.Type != "json_schema" || body.Text.Format.Name != "answer" || !body.Text.Format.Strict || body.Text.Format.Schema["type"] != "object" {
		t.Fatalf("structured text config = %#v", body.Text.Format)
	}
	if len(body.Tools) != 1 || body.Tools[0].Type != "web_search" {
		t.Fatalf("hosted tools = %#v", body.Tools)
	}
}

func TestResponses_UnsupportedHostedToolFailsBeforeNetwork(t *testing.T) {
	p := newResponsesClient(newResponsesHarness(t, nil))
	p.Capabilities.HostedTools = true
	p.HostedTools = []string{"computer_use"}
	_, err := p.buildResponsesRequest(provider.Request{})
	if err == nil || !strings.Contains(err.Error(), "supported: web_search") {
		t.Fatalf("error = %v", err)
	}
}

func TestResponses_ExplicitCapabilityProfileOverridesURLInference(t *testing.T) {
	p := NewResponses("sk-test", "https://gateway.example/v1", "model", 1024, time.Second, 0)
	if p.Capabilities.StructuredOutputs {
		t.Fatal("unknown gateway should start conservative")
	}
	if err := p.ConfigureCapabilityProfile("openai"); err != nil {
		t.Fatal(err)
	}
	if !p.Capabilities.StructuredOutputs || !p.Capabilities.EncryptedReasoning || !p.Capabilities.HostedTools {
		t.Fatalf("openai profile = %#v", p.Capabilities)
	}
	if err := p.ConfigureCapabilityProfile("compatible"); err != nil {
		t.Fatal(err)
	}
	if p.Capabilities.StructuredOutputs || p.Capabilities.StatefulResponses {
		t.Fatalf("compatible profile must be conservative: %#v", p.Capabilities)
	}
	if err := p.ConfigureCapabilityProfile("made-up"); err == nil {
		t.Fatal("unknown profile should fail during provider construction")
	}
}

func TestResponses_VendorProfilesDoNotOverclaimHostedTools(t *testing.T) {
	cases := []struct {
		baseURL     string
		stateful    bool
		hostedTools bool
	}{
		{"https://mtls.api.x.ai/v1", true, false},
		{"https://api.fireworks.ai/inference/v1", true, false},
		{"https://bedrock-runtime.us-east-1.amazonaws.com/openai/v1", true, false},
		{"https://ark.cn-beijing.volces.com/api/v3", true, true},
		{"https://open.bigmodel.cn/api/v1", true, true},
	}
	for _, tc := range cases {
		caps := detectResponsesCapabilities(tc.baseURL)
		if caps.StatefulResponses != tc.stateful || caps.HostedTools != tc.hostedTools {
			t.Errorf("%s: capabilities = %#v", tc.baseURL, caps)
		}
	}
}

func TestResponses_BigModelResponsesProfileIsPathSpecific(t *testing.T) {
	responses := detectResponsesCapabilities("https://open.bigmodel.cn/api/v1")
	if !responses.StatefulResponses || !responses.PromptCaching || !responses.HostedTools || responses.StructuredOutputs {
		t.Fatalf("BigModel Responses capabilities = %#v", responses)
	}
	chat := detectResponsesCapabilities("https://open.bigmodel.cn/api/coding/paas/v4")
	if chat.StatefulResponses || chat.HostedTools {
		t.Fatalf("Chat Completions path must not be mistaken for Responses: %#v", chat)
	}
}

func TestResponses_BigModelVisionUsesModelCapability(t *testing.T) {
	textOnly := NewResponses("test", "https://open.bigmodel.cn/api/v1", "glm-5.3", 1024, time.Second, 0)
	if got := textOnly.VisionCapability(); got != provider.VisionUnsupported {
		t.Fatalf("GLM 5.3 vision capability = %v, want unsupported", got)
	}
	vision := NewResponses("test", "https://open.bigmodel.cn/api/v1", "glm-5v-turbo", 1024, time.Second, 0)
	if got := vision.VisionCapability(); got != provider.VisionSupported {
		t.Fatalf("GLM 5V vision capability = %v, want supported", got)
	}
}

func TestResponses_StreamEmitsEncryptedReasoningAndProviderState(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.created","response":{"id":"resp_456","status":"in_progress"}}`,
		`{"type":"response.output_item.done","item":{"id":"rs_456","type":"reasoning","encrypted_content":"ciphertext"}}`,
		`{"type":"response.completed","response":{"id":"resp_456","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`,
	})
	p := newResponsesClient(h)
	p.StateMode = ResponsesStateProvider
	r, err := p.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	events := drainStream(t, r)
	var reasoning, state *provider.StreamEvent
	for i := range events {
		switch events[i].Type {
		case "redacted_thinking":
			reasoning = &events[i]
		case "provider_state":
			state = &events[i]
		}
	}
	if reasoning == nil || reasoning.TextDelta != "ciphertext" || reasoning.ProviderHint[responsesHintItemID] != "rs_456" {
		t.Fatalf("reasoning event = %#v", reasoning)
	}
	if state == nil || state.ProviderHint[responsesHintResponseID] != "resp_456" || state.ProviderHint[responsesHintStateKey] != p.stateKey() {
		t.Fatalf("state event = %#v", state)
	}
}

func TestResponses_CompletePreservesEncryptedReasoningAndProviderState(t *testing.T) {
	h := newResponsesHarness(t, nil)
	h.completeBody = `{"id":"resp_complete","status":"completed","output":[` +
		`{"id":"rs_complete","type":"reasoning","summary":[{"type":"summary_text","text":"brief"}],"encrypted_content":"ciphertext"},` +
		`{"type":"message","content":[{"type":"output_text","text":"answer"}]}` +
		`],"usage":{"input_tokens":7,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}`
	p := newResponsesClient(h)
	p.StateMode = ResponsesStateProvider
	result, err := p.Complete(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 4 {
		t.Fatalf("content = %#v, want thinking, encrypted reasoning, text and provider state", result.Content)
	}
	if result.Content[0].Type != "thinking" || result.Content[0].Text != "brief" {
		t.Fatalf("thinking = %#v", result.Content[0])
	}
	if result.Content[1].Type != "redacted_thinking" || result.Content[1].Data != "ciphertext" || result.Content[1].ProviderHint[responsesHintItemID] != "rs_complete" {
		t.Fatalf("encrypted reasoning = %#v", result.Content[1])
	}
	if result.Content[2].Type != "text" || result.Content[2].Text != "answer" {
		t.Fatalf("text = %#v", result.Content[2])
	}
	state := result.Content[3]
	if state.Type != "provider_state" || state.ProviderHint[responsesHintResponseID] != "resp_complete" || state.ProviderHint[responsesHintStateKey] != p.stateKey() {
		t.Fatalf("provider state = %#v", state)
	}
	if result.InputTokens != 5 || result.CacheReadInputTokens != 2 || result.OutputTokens != 3 {
		t.Fatalf("usage = %#v", result)
	}
}

func TestResponses_CompleteRefusalIsVisible(t *testing.T) {
	h := newResponsesHarness(t, nil)
	h.completeBody = `{"id":"resp_refusal","status":"completed","output":[` +
		`{"type":"message","content":[{"type":"refusal","refusal":"cannot comply"}]}` +
		`]}`
	p := newResponsesClient(h)
	result, err := p.Complete(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" || result.Content[0].Text != "cannot comply" {
		t.Fatalf("refusal content = %#v", result.Content)
	}
}

func TestResponses_ContextHistoryPolicyMatchesStateMode(t *testing.T) {
	p := newResponsesClient(newResponsesHarness(t, nil))
	p.Capabilities.EncryptedReasoning = true
	redacted := provider.ContentBlock{Type: "redacted_thinking"}
	if !p.ContextIncludesAssistantBlock(redacted) {
		t.Fatal("local encrypted reasoning is replayed and must count toward active context")
	}
	p.StateMode = ResponsesStateProvider
	if p.ContextIncludesAssistantBlock(redacted) {
		t.Fatal("provider-managed reasoning must not be counted as replayed local history")
	}
	if p.ContextIncludesAssistantBlock(provider.ContentBlock{Type: "provider_state"}) {
		t.Fatal("provider state marker is local metadata, not model context")
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

func TestResponses_StreamMapsFunctionItemIDToCallID(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.output_item.added","item":{"id":"fc_9","type":"function_call","call_id":"call_9","name":"Grep","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_9","delta":"{\"pattern\":\"x\"}"}`,
		`{"type":"response.output_item.done","item":{"id":"fc_9","type":"function_call","call_id":"call_9","name":"Grep","arguments":"{\"pattern\":\"x\"}"}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call_9","name":"Grep","arguments":"{\"pattern\":\"x\"}"}]}}`,
	})
	p := newResponsesClient(h)
	r, err := p.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	events := drainStream(t, r)
	for _, event := range events {
		switch event.Type {
		case "tool_use_start", "tool_input_delta", "tool_use_stop":
			if event.ToolUseID != "call_9" {
				t.Fatalf("%s id = %q, want call_9; events=%#v", event.Type, event.ToolUseID, events)
			}
		}
	}
}

func TestResponses_StreamPreservesTopLevelErrorMessage(t *testing.T) {
	h := newResponsesHarness(t, []string{`{"type":"error","code":"rate_limit_exceeded","message":"try again later"}`})
	p := newResponsesClient(h)
	r, err := p.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	events := drainStream(t, r)
	for _, event := range events {
		if event.Type == "error" && event.Err != nil && strings.Contains(event.Err.Error(), "try again later") && strings.Contains(event.Err.Error(), "rate_limit_exceeded") {
			return
		}
	}
	t.Fatalf("top-level Responses error message was lost: %#v", events)
}

func TestResponses_StreamRefusalIsVisibleWithoutDoneDuplication(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.refusal.delta","item_id":"msg_1","delta":"cannot comply"}`,
		`{"type":"response.refusal.done","item_id":"msg_1","refusal":"cannot comply"}`,
		`{"type":"response.completed","response":{"status":"completed","output":[]}}`,
	})
	p := newResponsesClient(h)
	r, err := p.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	events := drainStream(t, r)
	var text []string
	for _, event := range events {
		if event.Type == "text_delta" {
			text = append(text, event.TextDelta)
		}
	}
	if len(text) != 1 || text[0] != "cannot comply" {
		t.Fatalf("refusal text = %#v; events=%#v", text, events)
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

func TestResponses_StreamAcceptsNumericSequenceAndReasoningText(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_numeric","status":"in_progress"}}`,
		`{"type":"response.reasoning_text.delta","sequence_number":1,"delta":"internal"}`,
		`{"type":"response.output_text.delta","sequence_number":2,"delta":"answer"}`,
		`{"type":"response.completed","sequence_number":3,"response":{"id":"resp_numeric","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2}}}`,
	})
	p := newResponsesClient(h)
	stream, err := p.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	events := drainStream(t, stream)
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Type]++
	}
	if counts["thinking_delta"] != 1 || counts["text_delta"] != 1 || counts["message_delta"] != 1 {
		t.Fatalf("events = %#v, counts = %#v", events, counts)
	}
}

func TestResponses_StreamIncompleteMaxTokens(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.incomplete","response":{"id":"resp_incomplete","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
	})
	p := newResponsesClient(h)
	p.StateMode = ResponsesStateProvider
	r, err := p.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	events := drainStream(t, r)
	stop, stateID := "", ""
	for _, ev := range events {
		if ev.Type == "message_delta" {
			stop = ev.StopReason
		}
		if ev.Type == "provider_state" {
			stateID = ev.ProviderHint[responsesHintResponseID]
		}
	}
	if stop != "max_tokens" {
		t.Fatalf("stop = %q, want max_tokens", stop)
	}
	if stateID != "resp_incomplete" {
		t.Fatalf("provider state = %q, want resp_incomplete", stateID)
	}
}

func TestResponses_StreamIncompleteContentFilterIsNotStopSequence(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.output_text.delta","delta":"partial"}`,
		`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`,
	})
	p := newResponsesClient(h)
	stream, err := p.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	events := drainStream(t, stream)
	var stop string
	for _, event := range events {
		if event.Type == "message_delta" {
			stop = event.StopReason
		}
	}
	if stop != "content_filter" {
		t.Fatalf("stop = %q, want content_filter; events=%+v", stop, events)
	}
}

func TestResponses_CompleteIncompleteContentFilter(t *testing.T) {
	h := newResponsesHarness(t, nil)
	h.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`))
	})
	p := newResponsesClient(h)
	result, err := p.Complete(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != "content_filter" {
		t.Fatalf("stop = %q, want content_filter", result.StopReason)
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

func TestResponses_StreamEOFBeforeTerminalEventIsUnexpected(t *testing.T) {
	h := newResponsesHarness(t, []string{
		`{"type":"response.created","response":{"id":"resp_partial","status":"in_progress"}}`,
		`{"type":"response.output_text.delta","delta":"partial"}`,
	})
	p := newResponsesClient(h)
	r, err := p.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer r.Close()
	for {
		_, err = r.Recv()
		if err != nil {
			break
		}
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("terminal error = %v, want io.ErrUnexpectedEOF", err)
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

func TestResponses_CompletePreservesMalformedToolArgumentsForSafeRejection(t *testing.T) {
	out := `{"status":"completed","output":[
		{"type":"function_call","call_id":"c-bad","name":"Write","arguments":"{\"path\":\"a.go\",\"api_key\":\"do-not-echo\""}
	]}`
	h := newResponsesHarness(t, nil)
	h.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(out))
	})
	p := newResponsesClient(h)
	res, err := p.Complete(context.Background(), provider.Request{})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "tool_use" {
		t.Fatalf("content = %+v", res.Content)
	}
	if !res.Content[0].ToolInputMalformed || len(res.Content[0].ToolInput) != 0 {
		t.Fatalf("malformed arguments were silently collapsed: %+v", res.Content[0].ToolInput)
	}
	persisted, err := json.Marshal(res.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "do-not-echo") {
		t.Fatalf("malformed arguments leaked into persistence: %s", persisted)
	}
}
