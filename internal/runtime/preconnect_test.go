package runtime

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestPreconnect_FiresHEADToBaseHost — proves the warmup goroutine
// actually opens a connection to the host. We spin up an httptest
// server, point Preconnect at it, then wait briefly for the goroutine
// to fire.
func TestPreconnect_FiresHEADToBaseHost(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	Preconnect(srv.URL)

	// Goroutine fires async; allow up to 1s.
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&hits) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&hits); got == 0 {
		t.Errorf("Preconnect goroutine never reached server")
	}
}

// TestPreconnect_EmptyURLNoop — empty/missing baseURL must not crash
// and must not start a goroutine that could leak.
func TestPreconnect_EmptyURLNoop(t *testing.T) {
	Preconnect("")
	Preconnect("not a url")
	Preconnect("ftp://wrong-scheme")
	// No assertion needed — pass if no panic.
}

// TestPreconnect_BadHostFailsSilently — DNS failure must not propagate;
// preconnect is fire-and-forget. The real provider call will surface
// the error later in a user-actionable way.
func TestPreconnect_BadHostFailsSilently(t *testing.T) {
	Preconnect("http://this-host-does-not-exist-9876.invalid:65535/")
	// Wait past the dialer timeout so any goroutine panic surfaces.
	time.Sleep(100 * time.Millisecond)
}

// TestEarlyInput_NilSafe — non-TTY callers (CI, tests, expect scripts)
// receive nil from NewEarlyInput. All EarlyInput methods must handle
// nil gracefully.
func TestEarlyInput_NilSafe(t *testing.T) {
	var ei *EarlyInput
	ei.Stop() // nil receiver - must not panic
	if r := ei.Reader(); r == nil {
		t.Errorf("nil EarlyInput.Reader() should return os.Stdin, not nil")
	}
	if b := ei.CapturedBytes(); b != nil {
		t.Errorf("nil EarlyInput.CapturedBytes() should return nil; got %v", b)
	}
}

// TestEarlyInput_NewReturnsNilOnNonTTY — when stdin isn't a TTY (e.g.
// running tests under `go test`), NewEarlyInput must return nil rather
// than failing. Callers handle nil via the nil-safe methods above.
func TestEarlyInput_NewReturnsNilOnNonTTY(t *testing.T) {
	// In `go test`, os.Stdin is not a TTY → NewEarlyInput returns nil.
	if ei := NewEarlyInput(); ei != nil {
		t.Errorf("expected nil EarlyInput when stdin isn't a TTY; got %+v", ei)
	}
}

// TestIsAnthropicOrigin — gate for the anti-distillation startup
// warning. Real Anthropic origins return true; everything else false
// so the warning fires for third-party gateways.
func TestIsAnthropicOrigin(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"", true},                              // empty → default api.anthropic.com
		{"https://api.anthropic.com", true},     // canonical
		{"https://api.anthropic.com/v1", true},  // with path
		{"https://eu.api.anthropic.com", true},  // regional subdomain
		{"https://yunwu.ai", false},             // user's actual gateway
		{"https://openrouter.ai/api/v1", false}, // common alternative
		{"https://api.together.xyz/v1", false},  // Together
		{"https://api.minimax.chat", false},     // MiniMax direct
		{"http://localhost:9999", false},        // capture server
		{"not a url at all", false},             // garbage in
	}
	for _, tc := range cases {
		if got := isAnthropicOrigin(tc.url); got != tc.want {
			t.Errorf("isAnthropicOrigin(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
