package anthropic

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
)

const anthropicEchoedBearer = "Bearer ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"

func anthropicEchoedJWT() string {
	return "eyJhbGciOiJIUzI1NiJ9." + strings.Repeat("c", 12) + "." + strings.Repeat("d", 12)
}

func assertAnthropicErrorRedacted(t *testing.T, err error, required ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	text := err.Error()
	for _, secret := range []string{anthropicEchoedBearer, anthropicEchoedJWT()} {
		if strings.Contains(text, secret) {
			t.Fatalf("error leaked upstream credential %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("error did not expose a redaction marker: %s", text)
	}
	for _, want := range required {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q lost classification %q", text, want)
		}
	}
}

func assertAnthropicExactValuesRedacted(t *testing.T, err error, protected ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, value := range protected {
		if strings.Contains(err.Error(), value) {
			t.Fatal("provider error retained an exact request credential")
		}
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatal("provider error did not contain a redaction marker")
	}
}

func TestAnthropicExactRequestCredentialsRedactedAcrossCompleteHTTPAndStreamSSE(t *testing.T) {
	apiKey := "opaque-anthropic-api-value"
	t.Run("complete http error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"echo `+apiKey+`"}}`)
		}))
		defer server.Close()

		client := New(apiKey, server.URL, "claude-test", 64, time.Second, "")
		_, err := client.Complete(context.Background(), Request{})
		assertAnthropicExactValuesRedacted(t, err, apiKey)
	})

	accessToken := "oauthOpaqueAnthropic_28"
	newOAuthClient := func(baseURL string) *Anthropic {
		return NewOAuth(baseURL, "claude-test", 64, time.Second, "", func(context.Context) (string, error) {
			return accessToken, nil
		})
	}

	t.Run("stream http error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"token `+accessToken+`"}}`)
		}))
		defer server.Close()

		_, err := newOAuthClient(server.URL).Stream(context.Background(), Request{})
		assertAnthropicExactValuesRedacted(t, err, accessToken)
	})

	t.Run("stream sse error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"error","error":{"type":"authentication_error","message":"token `+accessToken+`"}}`+"\n\n")
		}))
		defer server.Close()

		stream, err := newOAuthClient(server.URL).Stream(context.Background(), Request{})
		if err != nil {
			t.Fatal("stream setup failed")
		}
		defer stream.Close()
		event, recvErr := stream.Recv()
		if recvErr != nil || event.Type != "error" {
			t.Fatal("expected an SSE error event")
		}
		assertAnthropicExactValuesRedacted(t, event.Err, accessToken)
	})
}

type anthropicOpaqueCloseBody struct {
	io.Reader
	err error
}

func (b *anthropicOpaqueCloseBody) Close() error { return b.err }

func TestAnthropicExactOAuthValueRedactedFromResolverAndCloseErrors(t *testing.T) {
	accessToken := "opaqueAnthropicResolver_81"
	t.Run("resolver returns value with error", func(t *testing.T) {
		client := NewOAuth("https://example.invalid", "claude-test", 64, time.Second, "", func(context.Context) (string, error) {
			return accessToken, errors.New("resolver echoed " + accessToken)
		})
		_, err := client.Stream(context.Background(), Request{})
		assertAnthropicExactValuesRedacted(t, err, accessToken)
	})

	t.Run("stream close", func(t *testing.T) {
		client := NewOAuth("https://example.invalid", "claude-test", 64, time.Second, "", func(context.Context) (string, error) {
			return accessToken, nil
		})
		client.httpClient = &http.Client{Transport: anthropicErrorRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: &anthropicOpaqueCloseBody{
					Reader: strings.NewReader(""),
					err:    errors.New("close echoed " + accessToken),
				},
			}, nil
		})}
		stream, err := client.Stream(context.Background(), Request{})
		if err != nil {
			t.Fatal("stream setup failed")
		}
		assertAnthropicExactValuesRedacted(t, stream.Close(), accessToken)
	})
}

func TestAnthropicHTTPErrorRedactsEchoedCredentials(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
		status int
	}{
		{name: "complete 4xx", status: http.StatusBadRequest},
		{name: "stream 5xx", stream: true, status: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Authorization: `+anthropicEchoedBearer+`; jwt `+anthropicEchoedJWT()+`"}}`)
			}))
			defer server.Close()

			client := New("not-a-real-key", server.URL, "claude-test", 64, time.Second, "")
			var err error
			if tt.stream {
				_, err = client.Stream(context.Background(), Request{})
			} else {
				_, err = client.Complete(context.Background(), Request{})
			}
			assertAnthropicErrorRedacted(t, err, "anthropic "+strconv.Itoa(tt.status), "invalid_request_error")
		})
	}
}

func TestAnthropicSSEErrorRedactsEchoedCredentials(t *testing.T) {
	body := "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Authorization: " + anthropicEchoedBearer + "; jwt " + anthropicEchoedJWT() + "\"}}\n\n"
	stream := newAnthropicStream(io.NopCloser(strings.NewReader(body)))
	defer stream.Close()
	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if event.Type != "error" {
		t.Fatalf("event type = %q, want error", event.Type)
	}
	assertAnthropicErrorRedacted(t, event.Err, "overloaded_error")
}

func TestAnthropicCompleteTreatsHTTP200ErrorEnvelopeAsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"Authorization: `+anthropicEchoedBearer+`; jwt `+anthropicEchoedJWT()+`"}}`)
	}))
	defer server.Close()

	client := New("not-a-real-key", server.URL, "claude-test", 64, time.Second, "")
	_, err := client.Complete(context.Background(), Request{})
	assertAnthropicErrorRedacted(t, err, "authentication_error")
}

type anthropicErrorRoundTripper func(*http.Request) (*http.Response, error)

func (fn anthropicErrorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type anthropicSecretNetError struct{ message string }

func (e anthropicSecretNetError) Error() string { return e.message }
func (anthropicSecretNetError) Timeout() bool   { return true }
func (anthropicSecretNetError) Temporary() bool { return true }
func (anthropicSecretNetError) Unwrap() error   { return context.DeadlineExceeded }

func TestAnthropicTransportErrorIsRedactedAndKeepsSafeClassification(t *testing.T) {
	client := New("not-a-real-key", "https://example.invalid", "claude-test", 64, time.Second, "")
	client.httpClient = &http.Client{Transport: anthropicErrorRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, anthropicSecretNetError{message: "Authorization: " + anthropicEchoedBearer + "; jwt " + anthropicEchoedJWT()}
	})}

	_, err := client.Complete(context.Background(), Request{})
	assertAnthropicErrorRedacted(t, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("redacted transport error lost deadline classification: %v", err)
	}
	if !transport.IsRetryExhausted(err) {
		t.Fatalf("redacted transport error lost retry exhaustion classification: %v", err)
	}
}

func TestAnthropicRedactedNetOpErrorKeepsNetworkClassification(t *testing.T) {
	secret := "opaque-anthropic-network-secret"
	err := redactAnthropicError(&net.OpError{
		Op:  "read",
		Net: "tcp",
		Err: errors.New("connection reset while carrying " + secret),
	}, secret)
	if strings.Contains(err.Error(), secret) {
		t.Fatal("redacted transport error leaked exact credential")
	}
	if !errors.Is(err, transport.ErrNetwork) {
		t.Fatalf("redacted net.OpError lost network classification: %v", err)
	}
}

type anthropicErrorReadCloser struct{}

func (anthropicErrorReadCloser) Read([]byte) (int, error) {
	return 0, errors.Join(io.ErrUnexpectedEOF, errors.New("Authorization: "+anthropicEchoedBearer+"; jwt "+anthropicEchoedJWT()))
}
func (anthropicErrorReadCloser) Close() error { return nil }

type anthropicPartialErrorReadCloser struct {
	body    []byte
	readErr error
	sent    bool
}

func (r *anthropicPartialErrorReadCloser) Read(dst []byte) (int, error) {
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	n := copy(dst, r.body)
	readErr := r.readErr
	if readErr == nil {
		readErr = errors.Join(io.ErrUnexpectedEOF, errors.New("Authorization: "+anthropicEchoedBearer+"; jwt "+anthropicEchoedJWT()))
	}
	return n, readErr
}

func TestAnthropicHTTPBodyReadFailureStillRetriesRetryableStatus(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			attempts := 0
			client := New("not-a-real-key", "https://example.invalid", "claude-test", 64, time.Second, "")
			client.httpClient = &http.Client{Transport: anthropicErrorRoundTripper(func(*http.Request) (*http.Response, error) {
				attempts++
				body := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Authorization: ` + anthropicEchoedBearer + `"}}`)
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     make(http.Header),
					Body: &anthropicPartialErrorReadCloser{
						body:    body,
						readErr: errors.New("gateway reader failed: Authorization: " + anthropicEchoedBearer),
					},
				}, nil
			})}

			var err error
			if stream {
				_, err = client.Stream(context.Background(), Request{})
			} else {
				_, err = client.Complete(context.Background(), Request{})
			}
			assertAnthropicErrorRedacted(t, err, "anthropic 503", "overloaded_error")
			if !transport.IsRetryExhausted(err) || attempts != 3 {
				t.Fatalf("retryable body-read attempts/classification = %d / %v", attempts, err)
			}
		})
	}
}
func (*anthropicPartialErrorReadCloser) Close() error { return nil }

func TestAnthropicHTTPBodyReadErrorKeepsStatusCodeAndIOClassification(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			attempts := 0
			client := New("not-a-real-key", "https://example.invalid", "claude-test", 64, time.Second, "")
			client.httpClient = &http.Client{Transport: anthropicErrorRoundTripper(func(*http.Request) (*http.Response, error) {
				attempts++
				body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"Authorization: ` + anthropicEchoedBearer + `"}}`)
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     make(http.Header),
					Body:       &anthropicPartialErrorReadCloser{body: body},
				}, nil
			})}

			var err error
			if stream {
				_, err = client.Stream(context.Background(), Request{})
			} else {
				_, err = client.Complete(context.Background(), Request{})
			}
			assertAnthropicErrorRedacted(t, err, "anthropic 400", "invalid_request_error")
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("body read error lost unexpected-EOF classification: %v", err)
			}
			if !transport.IsRetryExhausted(err) || attempts != 3 {
				t.Fatalf("body read attempts/classification = %d / %v", attempts, err)
			}
		})
	}
}

func TestAnthropicSSEReadErrorIsRedactedAndKeepsIOClassification(t *testing.T) {
	client := New("not-a-real-key", "https://example.invalid", "claude-test", 64, time.Second, "")
	client.httpClient = &http.Client{Transport: anthropicErrorRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       anthropicErrorReadCloser{},
		}, nil
	})}

	stream, err := client.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	event, err := stream.Recv()
	assertAnthropicErrorRedacted(t, err)
	assertAnthropicErrorRedacted(t, event.Err)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("redacted SSE read error lost unexpected-EOF classification: %v", err)
	}
}
