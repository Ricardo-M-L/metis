package anthropic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
)

type anthropicUsagePrivateWrapper struct{ cause error }

func (e anthropicUsagePrivateWrapper) Error() string {
	panic("private error text must not be evaluated")
}
func (e anthropicUsagePrivateWrapper) Unwrap() error { return e.cause }

func TestAnthropicUsageErrorRedactionKeepsSafeDurableClassification(t *testing.T) {
	const secret = "opaque-observer-cause-and-wrapper-secret"
	original := &transport.UsageObserverError{
		Stage: "begin", Code: "provider_usage_unresolved",
		Err: errors.Join(io.ErrUnexpectedEOF, errors.New(secret)),
	}
	err := redactAnthropicError(fmt.Errorf("%s: %w", secret, original))
	var safe *transport.UsageObserverError
	if !errors.As(err, &safe) {
		t.Fatalf("durable usage classification lost: %v", err)
	}
	if safe == original || safe.Err != nil || safe.Stage != "begin" || safe.Code != "provider_usage_unresolved" {
		t.Fatalf("expected a safe copied classification, got %+v", safe)
	}
	if errors.Unwrap(safe) != nil || errors.Is(err, io.ErrUnexpectedEOF) || transport.IsNetworkError(err) {
		t.Fatal("durable accounting failure exposed its callback cause or network retry semantics")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("durable accounting failure exposed arbitrary callback or wrapper text")
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if strings.Contains(current.Error(), secret) {
			t.Fatal("safe classification chain exposed arbitrary private text")
		}
	}
	calls := 0
	if retryErr := transport.RetryWithBackoff(context.Background(), 3, time.Millisecond, func() error {
		calls++
		return err
	}); retryErr == nil || calls != 1 {
		t.Fatalf("durable accounting failure retried: calls=%d error=%v", calls, retryErr)
	}
}

func TestAnthropicUsageErrorRedactionDoesNotEvaluatePrivateErrorText(t *testing.T) {
	err := redactAnthropicError(anthropicUsagePrivateWrapper{cause: &transport.UsageObserverError{
		Stage: "snapshot", Code: "provider_usage_unresolved", Err: errors.New("private callback text"),
	}})
	var safe *transport.UsageObserverError
	if !errors.As(err, &safe) || safe.Err != nil || safe.Stage != "snapshot" {
		t.Fatal("private wrapper did not retain safe typed accounting classification")
	}
}

func TestAnthropicUsageErrorRedactionDropsUnknownMetadataAndJoinedNetwork(t *testing.T) {
	const secret = "arbitrary-private-observer-metadata"
	original := &transport.UsageObserverError{Stage: secret, Code: secret, Err: errors.New(secret)}
	err := redactAnthropicError(errors.Join(original, io.ErrUnexpectedEOF))
	var safe *transport.UsageObserverError
	if !errors.As(err, &safe) || safe.Stage != "unknown" || safe.Code != "" || safe.Err != nil {
		t.Fatalf("unknown metadata was not removed from safe durable classification: %+v", safe)
	}
	if strings.Contains(err.Error(), secret) || transport.IsNetworkError(err) || errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("durable accounting error leaked metadata or allowed a joined network retry")
	}
}
