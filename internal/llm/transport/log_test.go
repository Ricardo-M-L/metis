package transport

// log_test.go — pin the always-on RoundTripper layer:
//   • request-id injection
//   • debug-log gating + format
//   • SSE pass-through (logger must NOT consume the stream)
//
// dump_test.go covers the heavy mechanism-B layer separately.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubRT is a tiny RoundTripper-shaped recorder: we want to assert on
// the request that reached the inner transport without a real network
// hop. The stub returns a fixed response.
type stubRT struct {
	last   *http.Request
	resp   *http.Response
	err    error
	called int
}

func (s *stubRT) RoundTrip(req *http.Request) (*http.Response, error) {
	s.called++
	s.last = req
	if s.err != nil {
		return nil, s.err
	}
	if s.resp != nil {
		return s.resp, nil
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"ok":true}`))),
		Header:     make(http.Header),
	}, nil
}

func newReq(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), "POST",
		"https://api.example.com/v1/messages?secret=hi",
		bytes.NewReader([]byte(`{"hello":"world"}`)))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer sk-test")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestLoggingTransport_InjectsRequestID(t *testing.T) {
	stub := &stubRT{}
	rt := WrapRoundTripper(stub, "anthropic", nil)
	req := newReq(t)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	got := stub.last.Header.Get(HeaderRequestID)
	if len(got) != 8 {
		t.Errorf("HeaderRequestID len = %d, want 8 hex chars; got %q", len(got), got)
	}
}

func TestLoggingTransport_PreservesExistingRequestID(t *testing.T) {
	stub := &stubRT{}
	rt := WrapRoundTripper(stub, "anthropic", nil)
	req := newReq(t)
	req.Header.Set(HeaderRequestID, "deadbeef")
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if got := stub.last.Header.Get(HeaderRequestID); got != "deadbeef" {
		t.Errorf("preserved id = %q; want deadbeef", got)
	}
}

func TestLoggingTransport_DebugLogGated(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "debug.log")
	t.Setenv("METIS_DEBUG_LOG", logPath)
	t.Setenv("METIS_DEBUG", "")
	closeLogForTest()

	stub := &stubRT{}
	rt := WrapRoundTripper(stub, "openai", nil)
	if _, err := rt.RoundTrip(newReq(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("debug=off should not create log file; got err=%v", err)
	}
}

func TestLoggingTransport_DebugLogWritten(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "debug.log")
	t.Setenv("METIS_DEBUG_LOG", logPath)
	t.Setenv("METIS_DEBUG", "1")
	closeLogForTest()
	defer closeLogForTest()

	stub := &stubRT{}
	rt := WrapRoundTripper(stub, "openai", nil)
	if _, err := rt.RoundTrip(newReq(t)); err != nil {
		t.Fatal(err)
	}
	// Force flush.
	closeLogForTest()
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	line := string(body)
	for _, want := range []string{"[http]", "openai", "POST", "/v1/messages", "200", "reqid="} {
		if !strings.Contains(line, want) {
			t.Errorf("debug log missing %q; line=%q", want, line)
		}
	}
	// Must NOT leak the bearer token.
	if strings.Contains(line, "sk-test") {
		t.Errorf("debug log leaked bearer token; line=%q", line)
	}
}

func TestLoggingTransport_LogsErrorPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_DEBUG_LOG", filepath.Join(dir, "debug.log"))
	t.Setenv("METIS_DEBUG", "1")
	closeLogForTest()
	defer closeLogForTest()

	stub := &stubRT{err: io.ErrUnexpectedEOF}
	rt := WrapRoundTripper(stub, "anthropic", nil)
	_, err := rt.RoundTrip(newReq(t))
	if err == nil {
		t.Fatal("expected error pass-through")
	}
	closeLogForTest()
	body, _ := os.ReadFile(filepath.Join(dir, "debug.log"))
	if !strings.Contains(string(body), "ERR:") {
		t.Errorf("error path should log ERR:; got %q", string(body))
	}
}

func TestLoggingTransport_DoesNotConsumeBody(t *testing.T) {
	// The logger layer is body-blind by design (dump.go does the
	// heavy lift). Pin the contract: after RoundTrip, the response
	// body must still be readable by the caller.
	stub := &stubRT{
		resp: &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewReader([]byte("hello"))),
			Header:     make(http.Header),
		},
	}
	rt := WrapRoundTripper(stub, "p", nil)
	resp, err := rt.RoundTrip(newReq(t))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" {
		t.Errorf("body consumed by logger; got %q", body)
	}
}

func TestIsTruthy(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"1", true}, {"true", true}, {"yes", true}, {"on", true},
		{"0", false}, {"false", false}, {"no", false}, {"off", false},
		{"", false}, {"  ", false},
		{"hello", true}, // anything-non-empty-non-zero rule
	}
	for _, c := range cases {
		if got := isTruthy(c.in); got != c.want {
			t.Errorf("isTruthy(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

func TestNewRequestID_Uniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		id := newRequestID()
		if len(id) != 8 {
			t.Errorf("len(id) = %d; want 8", len(id))
		}
		if seen[id] {
			t.Errorf("collision on iteration %d: %s", i, id)
		}
		seen[id] = true
	}
}

func TestNewHTTPClient_EndToEnd_RealServer(t *testing.T) {
	// Minimal end-to-end: hit a real httptest.Server through the
	// production constructor. Verifies no panics in the wiring,
	// status code propagates, request-id is on the wire.
	var capturedReqID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReqID = r.Header.Get(HeaderRequestID)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := NewHTTPClient(5*time.Second, "test-provider")
	req, _ := http.NewRequest("POST", srv.URL+"/messages", bytes.NewReader([]byte(`{}`)))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Errorf("body not valid JSON: %v", err)
	}
	if len(capturedReqID) != 8 {
		t.Errorf("server didn't see request-id header; got %q", capturedReqID)
	}
}
