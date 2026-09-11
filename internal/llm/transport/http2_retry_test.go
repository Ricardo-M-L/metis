package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func TestHTTP2PeerResetIsRecoverable(t *testing.T) {
	// Flush headers before aborting the server handler. This exercises Go's
	// real private http.http2StreamError from the response body, not a url.Error
	// around a failed handshake (which already satisfied net.Error).
	abort := make(chan struct{})
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
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
	client := s.Client()
	client.Timeout = 5 * time.Second
	response, err := client.Get(s.URL)
	close(abort)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, streamErr := io.ReadAll(response.Body)
	if streamErr == nil || response.ProtoMajor != 2 {
		t.Fatalf("wanted HTTP/2 body reset: protocol=%s error=%v", response.Proto, streamErr)
	}
	if got := streamErr.Error(); got != "stream error: stream ID 1; INTERNAL_ERROR; received from peer" {
		t.Fatalf("wrong peer error: %T %s", streamErr, got)
	}
	if !IsNetworkError(streamErr) || !shouldRetry(streamErr) || !shouldRecover(streamErr) {
		t.Fatalf("HTTP/2 peer reset lost retry classification: %T %v", streamErr, streamErr)
	}
	policy := RecoveryPolicy{MaxDuration: time.Minute, MaxAttempts: 3, MaxBackoff: time.Millisecond}
	calls := 0
	release, err := RetryWithRecovery(context.Background(), policy, func(context.Context) error {
		calls++
		if calls == 1 {
			return streamErr
		}
		return nil
	})
	release()
	if err != nil || calls != 2 {
		t.Fatalf("peer reset did not recover: calls=%d error=%v", calls, err)
	}
}

type lookalikeHTTP2StreamError struct {
	StreamID uint32
	Code     uint32
}

func (lookalikeHTTP2StreamError) Error() string {
	return "stream error: stream ID 1; INTERNAL_ERROR; received from peer"
}

func TestHTTP2RetryClassificationIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"internal", http2.StreamError{StreamID: 1, Code: http2.ErrCodeInternal}, true},
		{"refused", http2.StreamError{StreamID: 1, Code: http2.ErrCodeRefusedStream}, true},
		{"wrapped", fmt.Errorf("read: %w", http2.StreamError{StreamID: 1, Code: http2.ErrCodeInternal}), true},
		{"joined", errors.Join(errors.New("other"), http2.StreamError{StreamID: 1, Code: http2.ErrCodeInternal}), true},
		{"pointer", &http2.StreamError{StreamID: 1, Code: http2.ErrCodeInternal}, true},
		{"nil pointer", (*http2.StreamError)(nil), false},
		{"zero stream", http2.StreamError{Code: http2.ErrCodeInternal}, false},
		{"protocol", http2.StreamError{StreamID: 1, Code: http2.ErrCodeProtocol}, false},
		{"cancel", http2.StreamError{StreamID: 1, Code: http2.ErrCodeCancel}, false},
		{"security", http2.StreamError{StreamID: 1, Code: http2.ErrCodeInadequateSecurity}, false},
		{"unknown", http2.StreamError{StreamID: 1, Code: http2.ErrCode(0xff)}, false},
		{"text only", errors.New("stream error: stream ID 1; INTERNAL_ERROR; received from peer"), false},
		{"lookalike type", lookalikeHTTP2StreamError{StreamID: 1, Code: 2}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNetworkError(tc.err); got != tc.want {
				t.Fatalf("IsNetworkError(%T)=%t, want %t", tc.err, got, tc.want)
			}
		})
	}
	reset := http2.StreamError{StreamID: 1, Code: http2.ErrCodeInternal}
	if shouldRecover(errors.Join(context.Canceled, reset)) {
		t.Fatal("user cancellation must win over an HTTP/2 reset")
	}
	for _, code := range []int{400, 401, 403} {
		if shouldRecover(&HTTPStatusError{StatusCode: code, Err: reset}) {
			t.Fatalf("authoritative %d must not be retried because its body reset", code)
		}
	}
}
