package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/pkg/provider"
	"golang.org/x/net/http2"
)

func TestResponsesHTTP2BodyResetKeepsRecoveryClassification(t *testing.T) {
	abort := make(chan struct{})
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial draft\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-abort:
		case <-r.Context().Done():
		}
		panic(http.ErrAbortHandler)
	}))
	s.EnableHTTP2 = true
	s.StartTLS()
	defer s.Close()
	client := NewResponses("fixture-key", s.URL, "gpt-test", 64, 5*time.Second, 0)
	client.httpClient = s.Client()
	client.httpClient.Timeout = 5 * time.Second
	stream, err := client.Stream(context.Background(), provider.Request{Stream: true})
	if err != nil {
		close(abort)
		t.Fatal(err)
	}
	defer stream.Close()
	event, firstErr := stream.Recv()
	close(abort)
	if firstErr != nil || event.TextDelta != "partial draft" {
		t.Fatalf("missing pre-reset draft: event=%+v error=%v", event, firstErr)
	}
	event, err = stream.Recv()
	if err == nil || err.Error() != "stream error: stream ID 1; INTERNAL_ERROR; received from peer" {
		t.Fatalf("wrong stream failure: %T %v", err, err)
	}
	if !errors.Is(err, transport.ErrNetwork) || !errors.Is(event.Err, transport.ErrNetwork) {
		t.Fatalf("redaction lost the HTTP/2 network classification: %v", err)
	}
	session := transport.NewRecoverySession(transport.RecoveryPolicy{MaxDuration: time.Minute, MaxAttempts: 3, MaxBackoff: time.Second})
	if !session.RecordFailure(err) {
		t.Fatal("redacted HTTP/2 body reset was not recoverable")
	}
}

func TestResponsesHTTP2ClassificationDoesNotExposeWrappedCredentials(t *testing.T) {
	const secret = "private-test-credential"
	upstream := fmt.Errorf("credential=%s: %w", secret, http2.StreamError{StreamID: 1, Code: http2.ErrCodeInternal})
	err := redactResponsesError(upstream, secret)
	if strings.Contains(err.Error(), secret) || errors.Is(err, upstream) {
		t.Fatal("HTTP/2 classification exposed the credential-bearing original error")
	}
	if !errors.Is(err, transport.ErrNetwork) {
		t.Fatal("HTTP/2 classification was lost while redacting credentials")
	}
	var raw http2.StreamError
	if errors.As(err, &raw) {
		t.Fatal("redacted boundary must export only safe markers, not the original HTTP/2 error")
	}
}

func TestResponsesDefaultRecoveryPermanentStatusWinsOverBodyFailure(t *testing.T) {
	t.Setenv("METIS_RECOVERY_MAX_SECONDS", "")
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "")
	t.Setenv("METIS_RECOVERY_MAX_BACKOFF_SECONDS", "")
	for _, status := range []int{400, 401, 403} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("status_%d_stream_%t", status, streaming), func(t *testing.T) {
				calls := 0
				client := NewResponses("fixture-key", "https://example.invalid", "gpt-test", 64, time.Second, 0)
				client.httpClient = &http.Client{Transport: responsesErrorRoundTripper(func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: responsesErrorReadCloser{}}, nil
				})}
				var err error
				if streaming {
					_, err = client.Stream(context.Background(), provider.Request{Stream: true})
				} else {
					_, err = client.Complete(context.Background(), provider.Request{})
				}
				var statusErr *transport.HTTPStatusError
				if calls != 1 || !errors.As(err, &statusErr) || statusErr.StatusCode != status || transport.IsRetryExhausted(err) {
					t.Fatalf("permanent response was retried or lost its status: calls=%d error=%v", calls, err)
				}
			})
		}
	}
}
