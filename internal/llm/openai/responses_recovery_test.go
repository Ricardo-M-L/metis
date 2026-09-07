package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestResponsesRecoveryBeyondDefaultAttempts(t *testing.T) {
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "600")
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "5")
	for _, streamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[streamed], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				var successContext context.Context
				client := NewResponses("fake-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
				client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(req *http.Request) (*http.Response, error) {
					calls++
					if calls < 4 {
						return nil, io.ErrUnexpectedEOF
					}
					successContext = req.Context()
					body := `{"status":"completed","output":[]}`
					if streamed {
						body = "event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				if streamed {
					stream, err := client.Stream(context.Background(), provider.Request{})
					if err != nil {
						t.Fatalf("fourth attempt should recover: %v", err)
					}
					time.Sleep(601 * time.Second)
					if successContext.Err() != nil {
						t.Fatalf("recovery timer canceled successful stream: %v", successContext.Err())
					}
					if err := stream.Close(); err != nil {
						t.Fatal(err)
					}
					if successContext.Err() == nil {
						t.Fatal("Close must release recovered attempt context")
					}
				} else {
					if _, err := client.Complete(context.Background(), provider.Request{}); err != nil {
						t.Fatalf("fourth attempt should recover: %v", err)
					}
				}
				if calls != 4 {
					t.Fatalf("calls=%d, want 4", calls)
				}
			})
		})
	}
}

func TestResponsesRecoveryRejectsInvalidPolicyBeforeRequest(t *testing.T) {
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "invalid")
	client := NewResponses("fake-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
	calls := 0
	client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) { calls++; return nil, io.EOF })}
	_, err := client.Stream(context.Background(), provider.Request{})
	if err == nil || !strings.Contains(err.Error(), "METIS_RECOVERY_MAX_SECONDS") || calls != 0 {
		t.Fatalf("invalid policy: calls=%d err=%v", calls, err)
	}
}

func TestResponsesRecoveryPermanentStatusWinsOverTruncatedBody(t *testing.T) {
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "600")
	for _, status := range []int{400, 401, 403} {
		for _, streamed := range []bool{false, true} {
			calls := 0
			client := NewResponses("fake-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
			client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: &responsesPartialErrorReadCloser{body: []byte(`{"error":"invalid"}`), readErr: io.ErrUnexpectedEOF}}, nil
			})}
			var err error
			if streamed {
				_, err = client.Stream(context.Background(), provider.Request{})
			} else {
				_, err = client.Complete(context.Background(), provider.Request{})
			}
			if err == nil || calls != 1 || transport.IsRetryExhausted(err) {
				t.Fatalf("status=%d stream=%t calls=%d err=%v", status, streamed, calls, err)
			}
		}
	}
}

func TestResponsesRecoveryDialUsesWindowAndRedactsExhaustion(t *testing.T) {
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "2")
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "30")
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := NewResponses("secret-to-redact", "https://example.invalid", "gpt-test", 64, time.Second, 0)
		client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return nil, io.EOF
			}
			<-req.Context().Done()
			return nil, errors.Join(req.Context().Err(), errors.New("secret-to-redact"))
		})}
		start := time.Now()
		_, err := client.Stream(context.Background(), provider.Request{})
		if !transport.IsRetryExhausted(err) || calls != 2 || time.Since(start) != 2*time.Second {
			t.Fatalf("calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
		}
		if strings.Contains(err.Error(), "secret-to-redact") {
			t.Fatal("credential leaked")
		}
	})
}

func TestResponsesRecoverySuccessfulStatusTruncatedBody(t *testing.T) {
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "600")
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "3")
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := NewResponses("fake-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
		client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
			calls++
			var body io.ReadCloser = io.NopCloser(strings.NewReader(`{"status":"completed","output":[]}`))
			if calls == 1 {
				body = &responsesPartialErrorReadCloser{body: []byte(`{"status":`), readErr: io.ErrUnexpectedEOF}
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
		})}
		_, err := client.Complete(context.Background(), provider.Request{})
		if err != nil || calls != 2 {
			t.Fatalf("200 body recovery calls=%d err=%v", calls, err)
		}
	})
}

func TestResponsesRecoveryCodexCompleteDiscardsInterruptedDraft(t *testing.T) {
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "600")
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "3")
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := NewResponses("fake-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
		client.ProviderName = "openai-codex"
		client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
			calls++
			body := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"discarded-draft\"}\n\n"
			if calls == 2 {
				body = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"accepted-answer\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		response, err := client.Complete(context.Background(), provider.Request{})
		if err != nil || calls != 2 {
			t.Fatalf("Codex Complete recovery calls=%d err=%v", calls, err)
		}
		if len(response.Content) != 1 || response.Content[0].Text != "accepted-answer" {
			t.Fatalf("partial draft retained: %+v", response.Content)
		}
	})
}

func TestResponsesRecoveryStandaloneStateFallbackSharesBudget(t *testing.T) {
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "600")
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "3")
	for _, streamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[streamed], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client := NewResponses("fake-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
				client.StateMode = ResponsesStateProvider
				client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
					calls++
					if calls <= 2 {
						return nil, io.ErrUnexpectedEOF
					}
					status, body := 200, `{"status":"completed","output":[]}`
					if calls == 3 {
						status, body = 400, `{"error":{"code":"previous_response_not_found","message":"previous_response_id expired not found"}}`
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				req := provider.Request{Messages: stateRecoveryHistory(client)}
				var err error
				if streamed {
					var stream provider.StreamReader
					stream, err = client.Stream(context.Background(), req)
					if stream != nil {
						stream.Close()
					}
				} else {
					_, err = client.Complete(context.Background(), req)
				}
				if calls != 3 || !transport.IsRetryExhausted(err) {
					t.Fatalf("fallback got fresh budget: calls=%d err=%v", calls, err)
				}
			})
		})
	}
}

func TestResponsesRecoveryCodexPermanentStatusSurvivesRedaction(t *testing.T) {
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "600")
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "3")
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := NewResponses("fake-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
		client.ProviderName = "openai-codex"
		client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 400, Header: make(http.Header), Body: &responsesPartialErrorReadCloser{body: []byte(`{"error":"invalid"}`), readErr: io.ErrUnexpectedEOF}}, nil
		})}
		_, err := client.Complete(context.Background(), provider.Request{})
		if calls != 1 || err == nil || transport.IsRetryExhausted(err) {
			t.Fatalf("permanent status lost at redaction boundary: calls=%d err=%v", calls, err)
		}
	})
}
