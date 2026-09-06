package gemini

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeGemini is a minimal stand-in for Google's Generative Language
// API. Each test parameterizes the response body it'll send back so
// we can exercise text-only, tool-call, and multi-chunk paths without
// hitting the real endpoint.
type fakeGemini struct {
	responses []string // each element is one SSE frame body (without the "data: " prefix)
	gotKey    string
	gotPath   string
	gotBody   []byte
}

func (f *fakeGemini) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotKey = r.Header.Get("x-goog-api-key")
		f.gotPath = r.URL.Path + "?" + r.URL.RawQuery
		body, _ := io.ReadAll(r.Body)
		f.gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range f.responses {
			fmt.Fprintf(w, "data: %s\n\n", frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
}

// TestGemini_StreamText covers the happy path: server emits two text
// chunks plus a final finish marker; client must yield two text_delta
// events followed by message_delta + message_stop.
func TestGemini_StreamText(t *testing.T) {
	fake := &fakeGemini{responses: []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":" world"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}`,
	}}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	g := New("test-key", srv.URL, "gemini-2.5-pro", 0, 5*time.Second, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := g.Stream(ctx, Request{
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var got []StreamEvent
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got = append(got, ev)
	}

	wantTypes := []string{"text_delta", "text_delta", "message_delta", "message_stop"}
	if len(got) != len(wantTypes) {
		t.Fatalf("got %d events, want %d: %#v", len(got), len(wantTypes), got)
	}
	for i, ev := range got {
		if ev.Type != wantTypes[i] {
			t.Errorf("event[%d].Type = %q, want %q", i, ev.Type, wantTypes[i])
		}
	}
	// The first two events should carry the text chunks in order.
	if got[0].TextDelta != "Hello" {
		t.Errorf("event[0].TextDelta = %q, want %q", got[0].TextDelta, "Hello")
	}
	if got[1].TextDelta != " world" {
		t.Errorf("event[1].TextDelta = %q, want %q", got[1].TextDelta, " world")
	}
	// message_delta should carry the finish reason translated.
	if got[2].StopReason != "end_turn" {
		t.Errorf("message_delta.StopReason = %q, want end_turn", got[2].StopReason)
	}

	// Auth header forwarded to the server.
	if fake.gotKey != "test-key" {
		t.Errorf("x-goog-api-key did not match configured credential")
	}
	// SSE endpoint path resolved correctly.
	if !strings.Contains(fake.gotPath, ":streamGenerateContent") || !strings.Contains(fake.gotPath, "alt=sse") {
		t.Error("stream endpoint did not contain the expected operation and SSE selector")
	}
	if !strings.Contains(fake.gotPath, "gemini-2.5-pro") {
		t.Error("stream endpoint did not contain the configured model")
	}
}

// TestGemini_StreamToolCall verifies functionCall parts get translated
// into the tool_use_start / tool_input_delta / tool_use_stop sequence
// that the agent loop expects.
func TestGemini_StreamToolCall(t *testing.T) {
	fake := &fakeGemini{responses: []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"SF"}}}]},"finishReason":"TOOL_USE"}]}`,
	}}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	g := New("k", srv.URL, "gemini-2.5-flash", 0, 5*time.Second, 0)
	stream, err := g.Stream(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "weather?"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var seen []string
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		seen = append(seen, ev.Type)
		if ev.Type == "tool_use_start" && ev.ToolName != "get_weather" {
			t.Errorf("tool_use_start.ToolName = %q, want get_weather", ev.ToolName)
		}
		if ev.Type == "tool_input_delta" && !strings.Contains(ev.InputDelta, "SF") {
			t.Errorf("tool_input_delta.InputDelta missing SF: %q", ev.InputDelta)
		}
	}

	wantOrder := []string{"tool_use_start", "tool_input_delta", "tool_use_stop", "message_delta", "message_stop"}
	if len(seen) != len(wantOrder) {
		t.Fatalf("got %d events %v, want %d %v", len(seen), seen, len(wantOrder), wantOrder)
	}
	for i, ty := range seen {
		if ty != wantOrder[i] {
			t.Errorf("event[%d] = %q, want %q (full: %v)", i, ty, wantOrder[i], seen)
		}
	}
}

// TestGemini_RequestBodyShape verifies the wire format the request hits
// the API with — system instruction goes to systemInstruction, tools go
// under functionDeclarations, role mapping is correct.
func TestGemini_RequestBodyShape(t *testing.T) {
	fake := &fakeGemini{responses: []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`,
	}}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	g := New("k", srv.URL, "gemini-2.5-pro", 0, 5*time.Second, 0)
	stream, err := g.Stream(context.Background(), Request{
		System: "you are helpful",
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: RoleAssistant, Content: []ContentBlock{{Type: "text", Text: "hello"}}},
			{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "again"}}},
		},
		Tools: []ToolSpec{{Name: "x", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	stream.Close()

	body := string(fake.gotBody)
	if !strings.Contains(body, `"systemInstruction"`) {
		t.Errorf("expected systemInstruction in body; got %s", body)
	}
	if !strings.Contains(body, `"functionDeclarations"`) {
		t.Errorf("expected functionDeclarations in body; got %s", body)
	}
	// Role mapping: assistant → "model", user → "user".
	if !strings.Contains(body, `"role":"model"`) {
		t.Errorf("expected role=model in body; got %s", body)
	}
}

func TestGeminiErrorResponsesRedactAPIKey(t *testing.T) {
	const apiKey = "gemini-error-redaction-canary"
	tests := []struct {
		name string
		call func(*Gemini) error
	}{
		{
			name: "complete",
			call: func(g *Gemini) error {
				_, err := g.Complete(context.Background(), Request{})
				return err
			},
		},
		{
			name: "stream",
			call: func(g *Gemini) error {
				_, err := g.Stream(context.Background(), Request{})
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("x-goog-api-key") != apiKey {
					t.Error("x-goog-api-key did not match configured credential")
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"upstream echoed ` + apiKey + `"}}`))
			}))
			defer server.Close()

			g := New(apiKey, server.URL, "gemini-test", 0, 5*time.Second, 0)
			err := tc.call(g)
			if err == nil {
				t.Fatal("expected a non-2xx response error")
			}
			message := err.Error()
			if strings.Contains(message, apiKey) {
				t.Fatal("Gemini response error exposed the configured API key")
			}
			if !strings.Contains(message, "[REDACTED]") {
				t.Fatal("Gemini response error did not include a redaction marker")
			}
		})
	}
}

func assertGeminiRejectsRedirect(t *testing.T, call func(*Gemini) error) {
	t.Helper()
	const apiKey = "gemini-redirect-canary"
	targetRequests := make(chan bool, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests <- r.Header.Get("x-goog-api-key") != ""
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != apiKey {
			t.Error("initial Gemini request did not carry the configured credential")
		}
		http.Redirect(w, r, target.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	g := New(apiKey, redirector.URL, "gemini-test", 0, 5*time.Second, 0)
	if err := call(g); err == nil {
		t.Fatal("Gemini call followed or accepted a redirect")
	}
	select {
	case receivedKey := <-targetRequests:
		t.Errorf("redirect target received a request; credential_header_present=%t", receivedKey)
	default:
	}
}

func TestGeminiCompleteRejectsRedirect(t *testing.T) {
	assertGeminiRejectsRedirect(t, func(g *Gemini) error {
		_, err := g.Complete(context.Background(), Request{})
		return err
	})
}

func TestGeminiStreamRejectsRedirect(t *testing.T) {
	assertGeminiRejectsRedirect(t, func(g *Gemini) error {
		_, err := g.Stream(context.Background(), Request{})
		return err
	})
}

// TestGemini_MaxContextTokens spot-checks the per-model window pick.
// 2.5 family → 1M, gemini-pro classic → 32k, unknown → safe 1M default.
func TestGemini_MaxContextTokens(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"gemini-2.5-pro", 1_000_000},
		{"gemini-2.5-flash", 1_000_000},
		{"gemini-1.5-pro", 1_000_000},
		{"gemini-pro", 32_000},
		{"unknown-model", 1_000_000},
	}
	for _, tc := range cases {
		g := &Gemini{Model: tc.model}
		if got := g.MaxContextTokens(); got != tc.want {
			t.Errorf("MaxContextTokens(%q) = %d, want %d", tc.model, got, tc.want)
		}
	}
}
