package anthropic

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
)

type anthropicAuthCapture struct {
	mu      sync.Mutex
	headers []http.Header
}

func (c *anthropicAuthCapture) append(h http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headers = append(c.headers, h.Clone())
}

func newAnthropicAuthServer(t *testing.T, capture *anthropicAuthCapture) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.append(r.Header)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestAnthropicAPIKeyAndOAuthHeadersAreMutuallyExclusive(t *testing.T) {
	capture := &anthropicAuthCapture{}
	server := newAnthropicAuthServer(t, capture)

	apiKey := New("api-secret", server.URL, "claude-test", 256, 5*time.Second, "custom-beta")
	if _, err := apiKey.Complete(context.Background(), Request{}); err != nil {
		t.Fatalf("API-key Complete: %v", err)
	}

	resolverCalls := 0
	oauth := NewOAuth(server.URL, "claude-test", 256, 5*time.Second, "custom-beta,oauth-2025-04-20", func(context.Context) (string, error) {
		resolverCalls++
		return fmt.Sprintf("oauth-token-%d", resolverCalls), nil
	})
	if _, err := oauth.Complete(context.Background(), Request{}); err != nil {
		t.Fatalf("OAuth Complete: %v", err)
	}
	if _, err := oauth.Complete(context.Background(), Request{}); err != nil {
		t.Fatalf("second OAuth Complete: %v", err)
	}

	if len(capture.headers) != 3 {
		t.Fatalf("captured requests = %d, want 3", len(capture.headers))
	}
	keyHeader := capture.headers[0]
	if got := keyHeader.Get("x-api-key"); got != "api-secret" {
		t.Fatalf("API-key x-api-key = %q", got)
	}
	if got := keyHeader.Get("Authorization"); got != "" {
		t.Fatalf("API-key request unexpectedly sent Authorization = %q", got)
	}
	if got := keyHeader.Get("anthropic-beta"); got != "custom-beta" {
		t.Fatalf("API-key beta = %q", got)
	}

	for i, header := range capture.headers[1:] {
		if got := header.Get("x-api-key"); got != "" {
			t.Fatalf("OAuth request %d unexpectedly sent x-api-key", i+1)
		}
		wantAuth := fmt.Sprintf("Bearer oauth-token-%d", i+1)
		if got := header.Get("Authorization"); got != wantAuth {
			t.Fatalf("OAuth request %d Authorization = %q, want %q", i+1, got, wantAuth)
		}
		beta := header.Get("anthropic-beta")
		for _, required := range []string{"custom-beta", "oauth-2025-04-20", "claude-code-20250219"} {
			if !strings.Contains(beta, required) {
				t.Fatalf("OAuth beta %q missing %q", beta, required)
			}
		}
		if strings.Count(beta, "oauth-2025-04-20") != 1 {
			t.Fatalf("OAuth beta contains duplicate required value: %q", beta)
		}
		if got := header.Get("User-Agent"); got != "metis" {
			t.Fatalf("OAuth User-Agent = %q", got)
		}
		if got := header.Get("X-App"); got != "cli" {
			t.Fatalf("OAuth X-App = %q", got)
		}
		if got := header.Get("anthropic-dangerous-direct-browser-access"); got != "true" {
			t.Fatalf("OAuth browser-access compatibility header = %q", got)
		}
	}
	if resolverCalls != 2 {
		t.Fatalf("resolver calls = %d, want one per request", resolverCalls)
	}
}

func TestAnthropicOAuthStreamResolvesCredentialAtRequestTime(t *testing.T) {
	capture := &anthropicAuthCapture{}
	server := newAnthropicAuthServer(t, capture)
	calls := 0
	p := NewOAuth(server.URL, "claude-test", 256, 5*time.Second, "", func(context.Context) (string, error) {
		calls++
		return "stream-token", nil
	})
	stream, err := p.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	_, _ = stream.Recv()
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}
	if len(capture.headers) != 1 || capture.headers[0].Get("Authorization") != "Bearer stream-token" {
		t.Fatalf("stream OAuth headers = %#v", capture.headers)
	}
}

func TestAnthropicOAuthResolverErrorPropagates(t *testing.T) {
	want := fmt.Errorf("refresh unavailable")
	p := NewOAuth("http://127.0.0.1", "claude-test", 256, time.Second, "", func(context.Context) (string, error) {
		return "", want
	})
	_, err := p.Complete(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "refresh unavailable") {
		t.Fatalf("resolver error = %v", err)
	}
}
