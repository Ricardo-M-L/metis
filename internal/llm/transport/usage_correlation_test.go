package transport

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestUsageCorrelationNonceIsUniquePerPhysicalAttemptAndPreservesAgent(t *testing.T) {
	observer := &usageTestObserver{}
	req := usageTestHTTP(t, observer, false)
	req.Header.Set("User-Agent", "Metis/fixture metis-usage/stale")
	first, _, err := beginRequestUsage(req, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := beginRequestUsage(req, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if len(observer.metadata) != 2 {
		t.Fatal("physical attempts not reserved")
	}
	if first.Header.Get("User-Agent") != "Metis/fixture metis-usage/"+observer.metadata[0].ClientRequestID {
		t.Fatal("first wire ID differs from durable ID")
	}
	if second.Header.Get("User-Agent") != "Metis/fixture metis-usage/"+observer.metadata[1].ClientRequestID {
		t.Fatal("second wire ID differs from durable ID")
	}
	if observer.metadata[0].ClientRequestID == observer.metadata[1].ClientRequestID {
		t.Fatal("physical retry reused a nonce")
	}
	if req.Header.Get("User-Agent") != "Metis/fixture metis-usage/stale" {
		t.Fatal("caller request mutated")
	}
	if first.Header.Get("Authorization") != req.Header.Get("Authorization") {
		t.Fatal("provider auth header changed")
	}
}

func TestUsageCorrelationDoesNotChangeUnobservedChat(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "https://example.invalid/v1/messages", strings.NewReader(`{"model":"test"}`))
	req.Header.Set("User-Agent", "host-agent")
	clone, state, err := beginRequestUsage(req, "anthropic")
	if err != nil || state != nil || clone != req || clone.Header.Get("User-Agent") != "host-agent" {
		t.Fatal("unobserved request changed")
	}
}
