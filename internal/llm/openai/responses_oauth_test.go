package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

type responsesAuthCapture struct {
	mu      sync.Mutex
	headers []http.Header
	paths   []string
	bodies  []string
}

func (c *responsesAuthCapture) append(r *http.Request, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headers = append(c.headers, r.Header.Clone())
	c.paths = append(c.paths, r.URL.Path)
	c.bodies = append(c.bodies, body)
}

func newResponsesAuthServer(t *testing.T, capture *responsesAuthCapture) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		capture.append(r, body)
		if strings.Contains(body, `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_test","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestResponsesAPIKeyRetainsLegacyHeaders(t *testing.T) {
	capture := &responsesAuthCapture{}
	server := newResponsesAuthServer(t, capture)
	p := NewResponses("api-secret", server.URL, "gpt-test", 256, 5*time.Second, 0)
	if _, err := p.Complete(context.Background(), provider.Request{SessionID: "ordinary-session"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(capture.headers) != 1 {
		t.Fatalf("requests = %d", len(capture.headers))
	}
	if !strings.Contains(capture.bodies[0], `"max_output_tokens":256`) {
		t.Fatalf("API-key Responses must retain the configured output-token limit: %s", capture.bodies[0])
	}
	header := capture.headers[0]
	if got := header.Get("Authorization"); got != "Bearer api-secret" {
		t.Fatalf("Authorization = %q", got)
	}
	for _, name := range []string{"chatgpt-account-id", "originator", "OpenAI-Beta", "session-id", "x-client-request-id"} {
		if got := header.Get(name); got != "" {
			t.Fatalf("legacy API-key request unexpectedly sent %s=%q", name, got)
		}
	}
}

func TestCodexResponsesCompleteAndStreamUseDynamicOAuth(t *testing.T) {
	capture := &responsesAuthCapture{}
	server := newResponsesAuthServer(t, capture)
	calls := 0
	p := NewCodexResponses("gpt-test", 256, 5*time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
		calls++
		return ResponsesOAuthCredential{
			AccessToken: fmt.Sprintf("oauth-token-%d", calls),
			AccountID:   "account-123",
		}, nil
	})
	p.BaseURL = server.URL + "/backend-api/codex"

	const sessionID = "session-affinity-123"
	complete, err := p.Complete(context.Background(), provider.Request{Effort: EffortHigh, SessionID: sessionID, MaxTokens: 512})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(complete.Content) != 1 || complete.Content[0].Type != "text" || complete.Content[0].Text != "ok" {
		t.Fatalf("Complete aggregation = %+v", complete)
	}
	stream, err := p.Stream(context.Background(), provider.Request{Stream: true, SessionID: sessionID})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	_, _ = stream.Recv()

	if calls != 2 {
		t.Fatalf("resolver calls = %d, want one per outbound request", calls)
	}
	if len(capture.headers) != 2 {
		t.Fatalf("captured requests = %d", len(capture.headers))
	}
	for i, header := range capture.headers {
		wantAuth := fmt.Sprintf("Bearer oauth-token-%d", i+1)
		if got := header.Get("Authorization"); got != wantAuth {
			t.Fatalf("request %d Authorization = %q, want %q", i+1, got, wantAuth)
		}
		if got := header.Get("chatgpt-account-id"); got != "account-123" {
			t.Fatalf("request %d account id = %q", i+1, got)
		}
		if got := header.Get("originator"); got != "metis" {
			t.Fatalf("request %d originator = %q", i+1, got)
		}
		if got := header.Get("User-Agent"); got != "metis" {
			t.Fatalf("request %d User-Agent = %q", i+1, got)
		}
		if got := header.Get("OpenAI-Beta"); got != "responses=experimental" {
			t.Fatalf("request %d OpenAI-Beta = %q", i+1, got)
		}
		for _, name := range []string{"session-id", "x-client-request-id"} {
			if got := header.Get(name); got != sessionID {
				t.Fatalf("request %d %s = %q, want %q", i+1, name, got, sessionID)
			}
		}
		if capture.paths[i] != "/backend-api/codex/responses" {
			t.Fatalf("request %d path = %q", i+1, capture.paths[i])
		}
		if !strings.Contains(capture.bodies[i], `"store":false`) {
			t.Fatalf("request %d must force store=false: %s", i+1, capture.bodies[i])
		}
		if strings.Contains(capture.bodies[i], `"max_output_tokens"`) {
			t.Fatalf("request %d must omit the unsupported output-token limit: %s", i+1, capture.bodies[i])
		}
		if !strings.Contains(capture.bodies[i], `"instructions":"You are a helpful assistant."`) {
			t.Fatalf("request %d requires nonempty instructions: %s", i+1, capture.bodies[i])
		}
		if !strings.Contains(capture.bodies[i], `"parallel_tool_calls":true`) || !strings.Contains(capture.bodies[i], `"tool_choice":"auto"`) {
			t.Fatalf("request %d missing Codex tool controls: %s", i+1, capture.bodies[i])
		}
		if !strings.Contains(capture.bodies[i], `"prompt_cache_key":"`+sessionID+`"`) {
			t.Fatalf("request %d prompt cache key is not bound to the session: %s", i+1, capture.bodies[i])
		}
		if !strings.Contains(capture.bodies[i], `"text":{"verbosity":"low"}`) || strings.Contains(capture.bodies[i], `"format":{}`) {
			t.Fatalf("request %d has invalid Codex text options: %s", i+1, capture.bodies[i])
		}
		if i == 0 && !strings.Contains(capture.bodies[i], `"reasoning":{"effort":"high","summary":"auto"}`) {
			t.Fatalf("request %d missing Codex reasoning summary: %s", i+1, capture.bodies[i])
		}
	}
	if got := capture.headers[0].Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Complete Accept = %q", got)
	}
	if got := capture.headers[1].Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Stream Accept = %q", got)
	}
}

func TestCodexResponsesDefaults(t *testing.T) {
	p := NewCodexResponses("", 0, time.Second, 0, nil)
	if p.BaseURL != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("BaseURL = %q", p.BaseURL)
	}
	if p.Model != "gpt-5.5" {
		t.Fatalf("Model = %q", p.Model)
	}
	if p.Name() != "openai-codex" {
		t.Fatalf("Name = %q", p.Name())
	}
}

func TestCodexResponsesCredentialErrorsAreActionable(t *testing.T) {
	p := NewCodexResponses("gpt-test", 256, time.Second, 0, nil)
	_, err := p.Complete(context.Background(), provider.Request{})
	if err == nil || !strings.Contains(err.Error(), "metis login openai-codex") {
		t.Fatalf("missing credential error = %v", err)
	}

	p.OAuthTokenSource = func(context.Context) (ResponsesOAuthCredential, error) {
		return ResponsesOAuthCredential{}, fmt.Errorf("refresh unavailable")
	}
	_, err = p.Complete(context.Background(), provider.Request{})
	if err == nil || !strings.Contains(err.Error(), "refresh unavailable") {
		t.Fatalf("resolver error = %v", err)
	}
}
