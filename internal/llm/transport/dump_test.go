package transport

// dump_test.go — pin the heavy mechanism-B layer: full body capture,
// SSE parse, secret redaction, sessID-aware file routing.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// drainDumpFile waits up to 1 sec for the JSONL file to appear and
// have at least the requested number of entries (writes go through
// goroutines). Returns the parsed entries.
func drainDumpFile(t *testing.T, path string, want int) []dumpEntry {
	t.Helper()
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		closeDumpForTest() // flush
		body, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(body)), "\n")
			if len(lines) >= want && lines[0] != "" {
				out := make([]dumpEntry, 0, len(lines))
				for _, l := range lines {
					if l == "" {
						continue
					}
					var e dumpEntry
					if err := json.Unmarshal([]byte(l), &e); err != nil {
						t.Fatalf("invalid jsonl line: %v: %q", err, l)
					}
					out = append(out, e)
				}
				return out
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("dump file %s never reached %d entries", path, want)
	return nil
}

func setupDumpTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	t.Setenv("METIS_DUMP_PROMPTS", "1")
	t.Setenv("METIS_DEBUG", "")
	closeDumpForTest()
	closeLogForTest()
	t.Cleanup(func() {
		closeDumpForTest()
		closeLogForTest()
	})
	return filepath.Join(dir, "dump-prompts", "default.jsonl")
}

func TestDumpTransport_BypassWhenOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	t.Setenv("METIS_DUMP_PROMPTS", "")
	closeDumpForTest()

	stub := &stubRT{}
	rt := WrapRoundTripper(stub, "p", nil)
	if _, err := rt.RoundTrip(newReq(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dump-prompts")); !os.IsNotExist(err) {
		t.Errorf("dump=off should not create dump dir; got err=%v", err)
	}
}

func TestDumpTransport_RequestAndResponseLines(t *testing.T) {
	path := setupDumpTest(t)
	stub := &stubRT{}
	rt := WrapRoundTripper(stub, "anthropic", nil)
	if _, err := rt.RoundTrip(newReq(t)); err != nil {
		t.Fatal(err)
	}
	entries := drainDumpFile(t, path, 2)

	var req, resp *dumpEntry
	for i := range entries {
		switch entries[i].Type {
		case "request":
			req = &entries[i]
		case "response":
			resp = &entries[i]
		}
	}
	if req == nil {
		t.Fatal("missing request entry")
	}
	if resp == nil {
		t.Fatal("missing response entry")
	}
	if req.Provider != "anthropic" {
		t.Errorf("req.Provider = %q; want anthropic", req.Provider)
	}
	if req.Method != "POST" {
		t.Errorf("req.Method = %q", req.Method)
	}
	if !strings.Contains(req.URL, "/v1/messages") {
		t.Errorf("req.URL = %q; want path /v1/messages", req.URL)
	}
	if resp.Status != 200 {
		t.Errorf("resp.Status = %d; want 200", resp.Status)
	}
}

func TestDumpTransport_RedactsAuthHeader(t *testing.T) {
	path := setupDumpTest(t)
	stub := &stubRT{}
	rt := WrapRoundTripper(stub, "p", nil)
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "https://api.x/v1", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer sk-secret-DO-NOT-LEAK")
	req.Header.Set("X-API-Key", "another-secret")
	req.Header.Set("X-Goog-Api-Key", "gemini-secret-DO-NOT-LEAK")
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	entries := drainDumpFile(t, path, 2)
	for _, e := range entries {
		if e.Type != "request" {
			continue
		}
		raw, _ := json.Marshal(e.Headers)
		if strings.Contains(string(raw), "sk-secret-DO-NOT-LEAK") {
			t.Errorf("redaction failed; headers=%s", raw)
		}
		if strings.Contains(string(raw), "another-secret") {
			t.Errorf("X-API-Key not redacted; headers=%s", raw)
		}
		if strings.Contains(string(raw), "gemini-secret-DO-NOT-LEAK") {
			t.Errorf("X-Goog-Api-Key not redacted; headers=%s", raw)
		}
		if !strings.Contains(string(raw), "REDACTED") {
			t.Errorf("expected [REDACTED] marker; headers=%s", raw)
		}
	}
}

func TestDumpTransport_RedactsQueryString(t *testing.T) {
	path := setupDumpTest(t)
	stub := &stubRT{}
	rt := WrapRoundTripper(stub, "p", nil)
	req, _ := http.NewRequestWithContext(context.Background(), "POST",
		"https://api.x/v1?api_key=sk-leaky", bytes.NewReader([]byte("{}")))
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	entries := drainDumpFile(t, path, 2)
	for _, e := range entries {
		if strings.Contains(e.URL, "sk-leaky") {
			t.Errorf("URL leaked api_key: %s", e.URL)
		}
	}
}

func TestDumpTransport_BodyPreserved(t *testing.T) {
	// dump must restore req.Body so the inner transport can still read.
	path := setupDumpTest(t)
	stub := &stubRT{}
	rt := WrapRoundTripper(stub, "p", nil)
	bodyContent := `{"prompt":"hello"}`
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "https://x/y", bytes.NewReader([]byte(bodyContent)))
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	// What did the inner stub see?
	got, _ := io.ReadAll(stub.last.Body)
	if string(got) != bodyContent {
		t.Errorf("inner transport got %q; want %q", got, bodyContent)
	}
	// And the dump should have the parsed body.
	entries := drainDumpFile(t, path, 2)
	for _, e := range entries {
		if e.Type == "request" {
			var parsed map[string]any
			if err := json.Unmarshal(e.Body, &parsed); err != nil {
				t.Errorf("body not parsed JSON: %v", err)
			}
			if parsed["prompt"] != "hello" {
				t.Errorf("body wrong: %v", parsed)
			}
		}
	}
}

// TestDumpTransport_RedactsBodyTokens locks in the security.Redact
// integration: when the model exchange body contains a real-shaped
// secret (Anthropic key, GitHub PAT, OAuth access_token field), the
// dump file must NOT contain the secret value. Surrounding JSON
// structure must survive so the file is still valid JSONL.
func TestDumpTransport_RedactsBodyTokens(t *testing.T) {
	path := setupDumpTest(t)
	stub := &stubRT{}
	rt := WrapRoundTripper(stub, "p", nil)

	// Realistic-shape secrets the model could see / send.
	pat := "ghp_" + strings.Repeat("a", 36)
	body := `{"system":"you have key sk-` + "ant-api03-" + strings.Repeat("X", 93) + `AA","tools":[{"input":{"token":"` + pat + `"}}],"access_token":"oauth-secret-12345"}`
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "https://x/y", bytes.NewReader([]byte(body)))
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	entries := drainDumpFile(t, path, 2)
	for _, e := range entries {
		if e.Type != "request" {
			continue
		}
		raw := string(e.Body)
		if strings.Contains(raw, pat) {
			t.Errorf("GitHub PAT leaked into dump: %s", raw)
		}
		if strings.Contains(raw, "oauth-secret-12345") {
			t.Errorf("OAuth access_token leaked into dump: %s", raw)
		}
		// "[REDACTED]" should appear at least twice (once for the
		// token, once for the access_token JSON value).
		if strings.Count(raw, "[REDACTED]") < 2 {
			t.Errorf("expected ≥ 2 [REDACTED] markers, got body: %s", raw)
		}
	}
}

func TestDumpTransport_SSEParsing(t *testing.T) {
	path := setupDumpTest(t)
	sseBody := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"m1"}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	stub := &stubRT{
		resp: &http.Response{
			StatusCode: 200,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			Body: io.NopCloser(strings.NewReader(sseBody)),
		},
	}
	rt := WrapRoundTripper(stub, "anthropic", nil)
	resp, err := rt.RoundTrip(newReq(t))
	if err != nil {
		t.Fatal(err)
	}
	// Drain client-side — this is what triggers the dump-on-Close.
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "[DONE]") {
		t.Errorf("client reader didn't see full stream; got %q", body)
	}

	entries := drainDumpFile(t, path, 2)
	var resp2 *dumpEntry
	for i := range entries {
		if entries[i].Type == "response" {
			resp2 = &entries[i]
		}
	}
	if resp2 == nil {
		t.Fatal("no response entry in dump")
	}
	if !resp2.Stream {
		t.Errorf("Stream flag should be true on SSE")
	}
	// Body should be a parsed array of {event, data} chunks.
	var chunks []map[string]any
	if err := json.Unmarshal(resp2.Body, &chunks); err != nil {
		t.Fatalf("SSE chunks not array: %v: %s", err, string(resp2.Body))
	}
	if len(chunks) < 3 {
		t.Errorf("expected 3+ SSE chunks; got %d: %s", len(chunks), string(resp2.Body))
	}
	// First chunk should keep event=message_start.
	if chunks[0]["event"] != "message_start" {
		t.Errorf("first chunk event = %v; want message_start", chunks[0]["event"])
	}
}

func TestDumpTransport_SessionIDRouting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	t.Setenv("METIS_DUMP_PROMPTS", "1")
	closeDumpForTest()
	defer func() {
		closeDumpForTest()
		SetSessionID("")
	}()

	SetSessionID("session-A")
	stub := &stubRT{}
	rt := WrapRoundTripper(stub, "p", GlobalSessionID)
	if _, err := rt.RoundTrip(newReq(t)); err != nil {
		t.Fatal(err)
	}
	// Now switch and make another request.
	SetSessionID("session-B")
	if _, err := rt.RoundTrip(newReq(t)); err != nil {
		t.Fatal(err)
	}

	pathA := filepath.Join(dir, "dump-prompts", "session-A.jsonl")
	pathB := filepath.Join(dir, "dump-prompts", "session-B.jsonl")
	// Wait for both goroutines (request entry per RoundTrip) to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		closeDumpForTest()
		_, errA := os.Stat(pathA)
		_, errB := os.Stat(pathB)
		if errA == nil && errB == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, p := range []string{pathA, pathB} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected file %s; %v", p, err)
		}
	}
}

func TestDumpTransport_ErrorEntry(t *testing.T) {
	path := setupDumpTest(t)
	stub := &stubRT{err: io.ErrClosedPipe}
	rt := WrapRoundTripper(stub, "p", nil)
	if _, err := rt.RoundTrip(newReq(t)); err == nil {
		t.Fatal("expected error pass-through")
	}
	entries := drainDumpFile(t, path, 2)
	var hadError bool
	for _, e := range entries {
		if e.Type == "error" && e.Error != "" {
			hadError = true
		}
	}
	if !hadError {
		t.Errorf("no error entry in dump; got %+v", entries)
	}
}

func TestSetSessionID_RoundTrip(t *testing.T) {
	SetSessionID("abc-123")
	if got := GlobalSessionID(); got != "abc-123" {
		t.Errorf("GlobalSessionID() = %q; want abc-123", got)
	}
	SetSessionID("")
}
