package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

const responsesEchoedBearer = "Bearer ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"

func TestResponsesHTTPErrorRedactsJSONEncodedRequestCredentials(t *testing.T) {
	for _, encoded := range []string{`opaque\/vendor-test`, `op\u0061que\u002fvendor-test`} {
		for _, oauth := range []bool{false, true} {
			t.Run(encoded+"/oauth="+strconv.FormatBool(oauth), func(t *testing.T) {
				const key = "opaque/vendor-test"
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer "+key {
						t.Error("request did not carry the synthetic credential")
					}
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":{"code":"invalid_token","message":"rejected `+encoded+`"}}`)
				}))
				defer server.Close()
				var err error
				if oauth {
					client := NewCodexResponses("gpt-test", 64, time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
						return ResponsesOAuthCredential{AccessToken: key, AccountID: "synthetic-account"}, nil
					})
					client.BaseURL = server.URL
					_, err = client.Stream(context.Background(), provider.Request{Stream: true})
				} else {
					_, err = NewResponses(key, server.URL, "gpt-test", 64, time.Second, 0).Complete(context.Background(), provider.Request{})
				}
				if err == nil {
					t.Fatal("expected HTTP error")
				}
				var body struct {
					Error struct{ Code, Message string }
				}
				if decodeErr := json.Unmarshal([]byte(strings.TrimPrefix(err.Error(), "responses 401: ")), &body); decodeErr != nil {
					t.Fatalf("redacted response is not valid JSON: %v", decodeErr)
				}
				if strings.Contains(body.Error.Message, key) || !strings.Contains(body.Error.Message, "[REDACTED]") || body.Error.Code != "invalid_token" {
					t.Fatalf("credential leaked or error classification lost: %+v", body.Error)
				}
			})
		}
	}
}

func responsesEchoedJWT() string {
	return "eyJhbGciOiJIUzI1NiJ9." + strings.Repeat("a", 12) + "." + strings.Repeat("b", 12)
}

func assertResponsesErrorRedacted(t *testing.T, err error, required ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	text := err.Error()
	for _, secret := range []string{responsesEchoedBearer, responsesEchoedJWT()} {
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

func assertResponsesExactValuesRedacted(t *testing.T, err error, protected ...string) {
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

func TestResponsesExactRequestCredentialsRedactedAcrossCompleteHTTPAndStreamSSE(t *testing.T) {
	apiKey := "opaque-openai-api-value"
	t.Run("complete http error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"echo `+apiKey+`"}}`)
		}))
		defer server.Close()

		client := NewResponses(apiKey, server.URL, "gpt-test", 64, time.Second, 0)
		_, err := client.Complete(context.Background(), provider.Request{})
		assertResponsesExactValuesRedacted(t, err, apiKey)
	})

	accessToken := "oauthOpaque_92"
	accountID := "acctOpaque_73"
	newOAuthClient := func(baseURL string) *Responses {
		client := NewCodexResponses("gpt-test", 64, time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
			return ResponsesOAuthCredential{AccessToken: accessToken, AccountID: accountID}, nil
		})
		client.BaseURL = baseURL
		return client
	}

	t.Run("stream http error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_token","message":"token `+accessToken+` account `+accountID+`"}}`)
		}))
		defer server.Close()

		_, err := newOAuthClient(server.URL).Stream(context.Background(), provider.Request{Stream: true})
		assertResponsesExactValuesRedacted(t, err, accessToken, accountID)
	})

	t.Run("stream sse error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"error","code":"invalid_token","message":"token `+accessToken+` account `+accountID+`"}`+"\n\n")
		}))
		defer server.Close()

		stream, err := newOAuthClient(server.URL).Stream(context.Background(), provider.Request{Stream: true})
		if err != nil {
			t.Fatal("stream setup failed")
		}
		defer stream.Close()
		event, recvErr := stream.Recv()
		if recvErr != nil || event.Type != "error" {
			t.Fatal("expected an SSE error event")
		}
		assertResponsesExactValuesRedacted(t, event.Err, accessToken, accountID)
	})
}

type responsesOpaqueCloseBody struct {
	io.Reader
	err error
}

func (b *responsesOpaqueCloseBody) Close() error { return b.err }

func TestResponsesExactOAuthValuesRedactedFromResolverStatusAndCloseErrors(t *testing.T) {
	accessToken := "opaqueResolverToken_19"
	accountID := "opaqueResolverAccount_46"

	t.Run("resolver returns values with error", func(t *testing.T) {
		client := NewCodexResponses("gpt-test", 64, time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
			return ResponsesOAuthCredential{AccessToken: accessToken, AccountID: accountID}, errors.New("resolver echoed " + accessToken + " " + accountID)
		})
		_, err := client.Stream(context.Background(), provider.Request{Stream: true})
		assertResponsesExactValuesRedacted(t, err, accessToken, accountID)
	})

	t.Run("unknown sse status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.done","response":{"status":"`+accountID+`"}}`+"\n\n")
		}))
		defer server.Close()
		client := NewCodexResponses("gpt-test", 64, time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
			return ResponsesOAuthCredential{AccessToken: accessToken, AccountID: accountID}, nil
		})
		client.BaseURL = server.URL
		stream, err := client.Stream(context.Background(), provider.Request{Stream: true})
		if err != nil {
			t.Fatal("stream setup failed")
		}
		defer stream.Close()
		event, recvErr := stream.Recv()
		if recvErr != nil || event.Type != "error" {
			t.Fatal("expected an unknown-status SSE error event")
		}
		assertResponsesExactValuesRedacted(t, event.Err, accountID)
	})

	t.Run("stream close", func(t *testing.T) {
		client := NewCodexResponses("gpt-test", 64, time.Second, 0, func(context.Context) (ResponsesOAuthCredential, error) {
			return ResponsesOAuthCredential{AccessToken: accessToken, AccountID: accountID}, nil
		})
		client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: &responsesOpaqueCloseBody{
					Reader: strings.NewReader(""),
					err:    errors.New("close echoed " + accessToken + " " + accountID),
				},
			}, nil
		})}
		stream, err := client.Stream(context.Background(), provider.Request{Stream: true})
		if err != nil {
			t.Fatal("stream setup failed")
		}
		assertResponsesExactValuesRedacted(t, stream.Close(), accessToken, accountID)
	})
}

func TestResponsesHTTPErrorRedactsEchoedCredentials(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
		status int
	}{
		{name: "complete 4xx", status: http.StatusUnauthorized},
		{name: "stream 5xx", stream: true, status: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"Authorization: `+responsesEchoedBearer+`; jwt `+responsesEchoedJWT()+`"}}`)
			}))
			defer server.Close()

			client := NewResponses("not-a-real-key", server.URL, "gpt-test", 64, time.Second, 0)
			var err error
			if tt.stream {
				_, err = client.Stream(context.Background(), provider.Request{Stream: true})
			} else {
				_, err = client.Complete(context.Background(), provider.Request{})
			}
			assertResponsesErrorRedacted(t, err, "responses "+strconv.Itoa(tt.status), "invalid_api_key")
		})
	}
}

func TestResponsesSSEErrorsRedactCredentialsAndKeepCode(t *testing.T) {
	tests := []struct {
		name  string
		event string
		code  string
	}{
		{
			name:  "error event",
			event: `{"type":"error","code":"rate_limit_exceeded","message":"Authorization: ` + responsesEchoedBearer + `; jwt ` + responsesEchoedJWT() + `"}`,
			code:  "rate_limit_exceeded",
		},
		{
			name:  "failed terminal",
			event: `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"Authorization: ` + responsesEchoedBearer + `; jwt ` + responsesEchoedJWT() + `"}}}`,
			code:  "server_error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newResponsesHarness(t, []string{tt.event})
			stream, err := newResponsesClient(h).Stream(context.Background(), provider.Request{Stream: true})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			defer stream.Close()
			event, recvErr := stream.Recv()
			if recvErr != nil {
				t.Fatalf("Recv: %v", recvErr)
			}
			if event.Type != "error" {
				t.Fatalf("event type = %q, want error", event.Type)
			}
			assertResponsesErrorRedacted(t, event.Err, tt.code)
		})
	}
}

func TestResponsesCompleteEnvelopeErrorRedactsCredentialsAndKeepsCode(t *testing.T) {
	h := newResponsesHarness(t, nil)
	h.completeBody = `{"status":"failed","error":{"code":"invalid_request_error","message":"Authorization: ` + responsesEchoedBearer + `; jwt ` + responsesEchoedJWT() + `"}}`
	_, err := newResponsesClient(h).Complete(context.Background(), provider.Request{})
	assertResponsesErrorRedacted(t, err, "invalid_request_error")
}

func TestResponsesCompleteTreatsCodeOnlyAndBareFailedEnvelopesAsErrors(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		required []string
	}{
		{
			name:     "code only error",
			body:     `{"status":"failed","error":{"code":"overloaded_error"}}`,
			required: []string{"responses request failed", "overloaded_error"},
		},
		{
			name:     "failed without error object",
			body:     `{"status":"failed"}`,
			required: []string{"responses request failed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newResponsesHarness(t, nil)
			h.completeBody = tt.body
			_, err := newResponsesClient(h).Complete(context.Background(), provider.Request{})
			if err == nil {
				t.Fatalf("failed envelope was treated as success: %s", tt.body)
			}
			for _, want := range tt.required {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q lost %q", err, want)
				}
			}
		})
	}
}

type responsesErrorRoundTripper func(*http.Request) (*http.Response, error)

func (fn responsesErrorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type responsesSecretNetError struct{ message string }

func (e responsesSecretNetError) Error() string { return e.message }
func (responsesSecretNetError) Timeout() bool   { return true }
func (responsesSecretNetError) Temporary() bool { return true }
func (responsesSecretNetError) Unwrap() error   { return context.DeadlineExceeded }

func TestResponsesTransportErrorIsRedactedAndKeepsSafeClassification(t *testing.T) {
	client := NewResponses("not-a-real-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
	attempts := 0
	client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, responsesSecretNetError{message: "Authorization: " + responsesEchoedBearer + "; jwt " + responsesEchoedJWT()}
	})}

	_, err := client.Complete(context.Background(), provider.Request{})
	assertResponsesErrorRedacted(t, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("redacted transport error lost deadline classification: %v", err)
	}
	if !transport.IsRetryExhausted(err) {
		t.Fatalf("redacted transport error lost retry exhaustion classification: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("Complete transport attempts = %d, want 3", attempts)
	}
}

func TestResponsesRedactedNetOpErrorKeepsNetworkClassification(t *testing.T) {
	secret := "opaque-network-secret"
	err := redactResponsesError(&net.OpError{
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

func TestResponsesCompleteRetryableHTTPErrorRedactsAndKeepsStatusCode(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"overloaded_error","message":"Authorization: `+responsesEchoedBearer+`; jwt `+responsesEchoedJWT()+`"}}`)
	}))
	defer server.Close()

	client := NewResponses("not-a-real-key", server.URL, "gpt-test", 64, time.Second, 0)
	_, err := client.Complete(context.Background(), provider.Request{})
	assertResponsesErrorRedacted(t, err, "responses 503", "overloaded_error")
	if !transport.IsRetryExhausted(err) {
		t.Fatalf("retryable HTTP error lost retry exhaustion classification: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("Complete HTTP attempts = %d, want 3", attempts)
	}
}

func TestResponsesCompleteHonorsRetryAfterFor429And5xx(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		after  time.Duration
	}{
		{name: "429", status: http.StatusTooManyRequests, after: 2 * time.Second},
		{name: "503", status: http.StatusServiceUnavailable, after: 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				client := NewResponses("not-a-real-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
				client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
					attempts++
					if attempts == 1 {
						return &http.Response{
							StatusCode: tc.status,
							Header:     http.Header{"Retry-After": []string{strconv.Itoa(int(tc.after / time.Second))}},
							Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"overloaded_error"}}`)),
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"status":"completed","output":[]}`)),
					}, nil
				})}

				started := time.Now()
				if _, err := client.Complete(context.Background(), provider.Request{}); err != nil {
					t.Fatal(err)
				}
				if attempts != 2 {
					t.Fatalf("complete attempts = %d, want 2", attempts)
				}
				if elapsed := time.Since(started); elapsed != tc.after {
					t.Fatalf("Retry-After delay = %v, want %v", elapsed, tc.after)
				}
			})
		})
	}
}

func TestResponsesStreamRetriesOrdinaryRetryableStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		retryAfter string
		wantDelay  time.Duration
	}{
		{name: "429", status: http.StatusTooManyRequests, wantDelay: 5 * time.Second},
		{name: "503", status: http.StatusServiceUnavailable, wantDelay: 500 * time.Millisecond},
		{name: "retry after", status: http.StatusServiceUnavailable, retryAfter: "3", wantDelay: 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				client := NewResponses("not-a-real-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
				client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
					attempts++
					if attempts == 1 {
						return &http.Response{
							StatusCode: tc.status,
							Header:     http.Header{"Retry-After": []string{tc.retryAfter}},
							Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"overloaded_error","message":"try later"}}`)),
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				})}
				started := time.Now()
				stream, err := client.Stream(context.Background(), provider.Request{Stream: true})
				if err != nil {
					t.Fatal(err)
				}
				_ = stream.Close()
				if attempts != 2 {
					t.Fatalf("stream attempts = %d, want 2", attempts)
				}
				elapsed := time.Since(started)
				if tc.retryAfter != "" {
					if elapsed != tc.wantDelay {
						t.Fatalf("Retry-After delay = %v, want %v", elapsed, tc.wantDelay)
					}
				} else if min, max := tc.wantDelay*4/5, tc.wantDelay*6/5; elapsed < min || elapsed > max {
					t.Fatalf("jittered retry delay = %v, want range [%v, %v]", elapsed, min, max)
				}
			})
		})
	}
}

func TestResponsesStreamRetryExhaustionRedactsStatusBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		client := NewResponses("not-a-real-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
		client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
			attempts++
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"error":{"code":"overloaded_error","message":"Authorization: ` +
					responsesEchoedBearer + `"}}`)),
			}, nil
		})}
		_, err := client.Stream(context.Background(), provider.Request{Stream: true})
		assertResponsesErrorRedacted(t, err, "responses 503", "overloaded_error")
		if !transport.IsRetryExhausted(err) || attempts != 3 {
			t.Fatalf("retry exhaustion = attempts %d, error %v", attempts, err)
		}
	})
}

type responsesErrorReadCloser struct{}

func (responsesErrorReadCloser) Read([]byte) (int, error) {
	return 0, errors.Join(io.ErrUnexpectedEOF, errors.New("Authorization: "+responsesEchoedBearer+"; jwt "+responsesEchoedJWT()))
}
func (responsesErrorReadCloser) Close() error { return nil }

type responsesPartialErrorReadCloser struct {
	body    []byte
	readErr error
	sent    bool
}

func (r *responsesPartialErrorReadCloser) Read(dst []byte) (int, error) {
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	n := copy(dst, r.body)
	readErr := r.readErr
	if readErr == nil {
		readErr = errors.Join(io.ErrUnexpectedEOF, errors.New("Authorization: "+responsesEchoedBearer+"; jwt "+responsesEchoedJWT()))
	}
	return n, readErr
}

func TestResponsesHTTPBodyReadFailureStillRetriesRetryableStatus(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			attempts := 0
			client := NewResponses("not-a-real-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
			client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
				attempts++
				body := []byte(`{"error":{"code":"overloaded_error","message":"Authorization: ` + responsesEchoedBearer + `"}}`)
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     make(http.Header),
					Body: &responsesPartialErrorReadCloser{
						body:    body,
						readErr: errors.New("gateway reader failed: Authorization: " + responsesEchoedBearer),
					},
				}, nil
			})}

			var err error
			if stream {
				_, err = client.Stream(context.Background(), provider.Request{Stream: true})
			} else {
				_, err = client.Complete(context.Background(), provider.Request{})
			}
			assertResponsesErrorRedacted(t, err, "responses 503", "overloaded_error")
			if !transport.IsRetryExhausted(err) || attempts != 3 {
				t.Fatalf("retryable body-read attempts/classification = %d / %v", attempts, err)
			}
		})
	}
}
func (*responsesPartialErrorReadCloser) Close() error { return nil }

func TestResponsesHTTPBodyReadErrorKeepsStatusCodeAndIOClassification(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			attempts := 0
			client := NewResponses("not-a-real-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
			client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
				attempts++
				body := []byte(`{"error":{"code":"invalid_request_error","message":"Authorization: ` + responsesEchoedBearer + `"}}`)
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     make(http.Header),
					Body:       &responsesPartialErrorReadCloser{body: body},
				}, nil
			})}

			var err error
			if stream {
				_, err = client.Stream(context.Background(), provider.Request{Stream: true})
			} else {
				_, err = client.Complete(context.Background(), provider.Request{})
			}
			assertResponsesErrorRedacted(t, err, "responses 400", "invalid_request_error")
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("body read error lost unexpected-EOF classification: %v", err)
			}
			if !transport.IsRetryExhausted(err) || attempts != 3 {
				t.Fatalf("body read attempts/classification = %d / %v", attempts, err)
			}
		})
	}
}

func TestResponsesSSEReadErrorIsRedactedAndKeepsIOClassification(t *testing.T) {
	client := NewResponses("not-a-real-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
	client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       responsesErrorReadCloser{},
		}, nil
	})}

	stream, err := client.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	event, err := stream.Recv()
	assertResponsesErrorRedacted(t, err)
	assertResponsesErrorRedacted(t, event.Err)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("redacted SSE read error lost unexpected-EOF classification: %v", err)
	}
}
