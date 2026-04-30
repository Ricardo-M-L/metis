package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/channels"
)

func TestTelegram_RetriesOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	a := &Adapter{
		Token:       "tok",
		HTTPClient:  srv.Client(),
		BaseURL:     srv.URL,
		MaxRetries:  4,
		BaseBackoff: 1 * time.Millisecond, // fast test
	}
	if err := a.Send(context.Background(), "@chat", channels.Message{Text: "hi"}); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", calls.Load())
	}
}

func TestTelegram_NoRetryOn400(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(400)
	}))
	defer srv.Close()

	a := &Adapter{Token: "tok", HTTPClient: srv.Client(), BaseURL: srv.URL, MaxRetries: 3, BaseBackoff: 1 * time.Millisecond}
	if err := a.Send(context.Background(), "@chat", channels.Message{Text: "hi"}); err == nil {
		t.Error("400 should not retry to success")
	}
	if calls.Load() != 1 {
		t.Errorf("400 should not retry; got %d calls", calls.Load())
	}
}

func TestTelegram_URLContainsToken(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(200)
	}))
	defer srv.Close()

	a := &Adapter{Token: "test-token", HTTPClient: srv.Client(), BaseURL: srv.URL, MaxRetries: 1, BaseBackoff: time.Millisecond}
	a.Send(context.Background(), "@chat", channels.Message{Text: "hi"})
	if !strings.Contains(gotPath, "/bottest-token/sendMessage") {
		t.Errorf("URL token missing: %q", gotPath)
	}
}

func TestTelegram_DefaultChatIDFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	a := New("tok", "@default-chat")
	a.HTTPClient = srv.Client()
	a.BaseURL = srv.URL
	a.MaxRetries = 1
	a.BaseBackoff = time.Millisecond

	if err := a.Send(context.Background(), "", channels.Message{Text: "x"}); err != nil {
		t.Fatal(err)
	}
}
