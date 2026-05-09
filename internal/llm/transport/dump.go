package transport

// dump.go — full request/response body dump (mechanism B in the
// claude-code RoundTripper layout). Mirrors
// services/api/dumpPrompts.ts:146 (createDumpPromptsFetch).
//
// Off by default. Turned on by METIS_DUMP_PROMPTS=1 (or *DUMP*).
// When on, every request/response pair is appended to:
//
//   ~/.metis/dump-prompts/<sid>.jsonl
//
// One line per HTTP exchange, two entries per exchange (one
// `request` line written before send, one `response` line on
// completion). SSE responses are parsed into a chunks array so the
// dump is replayable without raw bytes mangling.
//
// Differences from claude-code's dumpPrompts.ts:
//
//   • No init-fingerprint optimisation. claude-code dedupes the
//     ~MB system prompt across turns; metis user runs are short
//     enough that we just write the whole thing — the goroutine
//     handles the latency.
//   • No 5-entry in-memory cache (claude-code uses it for /issue
//     bug reports). metis has no equivalent yet, and adding the
//     state weight isn't justified by current tooling.
//   • No USER_TYPE gating — metis users opting in via env are by
//     definition allowed to dump.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// dumpTransport is the RoundTripper that writes the JSONL.
type dumpTransport struct {
	inner    http.RoundTripper
	provider string
	// sessIDFn is queried at request-time so /new mid-process flips
	// to a new dump file. nil → all entries land under "default".
	sessIDFn SessionIDFn
}

// resolveSessID picks the sessID for one request, falling back to
// "default" when the closure isn't wired or returns empty.
func (t *dumpTransport) resolveSessID() string {
	if t.sessIDFn == nil {
		return "default"
	}
	id := t.sessIDFn()
	if id == "" {
		return "default"
	}
	return id
}

// dumpEntry is the wire shape for each JSONL line.
type dumpEntry struct {
	Type      string          `json:"type"` // "request" | "response" | "error"
	Timestamp string          `json:"timestamp"`
	Provider  string          `json:"provider"`
	ReqID     string          `json:"reqid,omitempty"`
	Method    string          `json:"method,omitempty"`
	URL       string          `json:"url,omitempty"`
	Status    int             `json:"status,omitempty"`
	Headers   map[string]any  `json:"headers,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
	Error     string          `json:"error,omitempty"`
	Stream    bool            `json:"stream,omitempty"`
	ElapsedMS int64           `json:"elapsed_ms,omitempty"`
}

// RoundTrip — capture body before send (cheap), forward, capture body
// after (clone via io.TeeReader so we don't break the streaming case).
func (t *dumpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !isDumpEnabled() {
		// Bypass entirely — no body cloning, no goroutine, just inner.
		return t.inner.RoundTrip(req)
	}

	reqID := req.Header.Get(HeaderRequestID)
	tsStart := time.Now()
	sessID := t.resolveSessID()

	// 1. capture request body. Reading-and-restoring is the
	// stdlib idiom: r.Body must be re-readable for the actual
	// transport that follows.
	var reqBody []byte
	if req.Body != nil {
		buf, err := io.ReadAll(req.Body)
		if err == nil {
			reqBody = buf
			req.Body = io.NopCloser(bytes.NewReader(buf))
			req.ContentLength = int64(len(buf))
		}
	}

	// 2. Async write of the request entry. We don't wait for the
	// goroutine — the model call is on the critical path.
	dumpWG.Add(1)
	go appendDump(sessID, dumpEntry{
		Type:      "request",
		Timestamp: tsStart.UTC().Format(time.RFC3339Nano),
		Provider:  t.provider,
		ReqID:     reqID,
		Method:    req.Method,
		URL:       safeURL(req),
		Headers:   redactHeaders(req.Header),
		Body:      jsonOrRaw(reqBody),
	})

	// 3. dispatch.
	resp, err := t.inner.RoundTrip(req)

	if err != nil {
		dumpWG.Add(1)
		go appendDump(sessID, dumpEntry{
			Type:      "error",
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Provider:  t.provider,
			ReqID:     reqID,
			Method:    req.Method,
			URL:       safeURL(req),
			Error:     err.Error(),
			ElapsedMS: time.Since(tsStart).Milliseconds(),
		})
		return resp, err
	}

	// 4. capture response. SSE (text/event-stream) requires a
	// streaming-safe wrapper — we tee bytes into a buffer as the
	// caller drains them, then dump on close. Non-streaming JSON
	// responses get fully buffered up-front (resp body small, no
	// reason to do the tee dance).
	if isSSE(resp) {
		buf := &bytes.Buffer{}
		body := resp.Body
		// Cap the dump buffer so a runaway server doesn't OOM the
		// agent. 16 MiB is generous — a Claude opus full-context
		// stream tops out around 4 MiB.
		const cap = 16 << 20
		resp.Body = &teeCloser{
			r: io.TeeReader(body, capWriter{buf, cap}),
			c: body,
			onClose: func() {
				dumpWG.Add(1)
				go appendDump(sessID, dumpEntry{
					Type:      "response",
					Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
					Provider:  t.provider,
					ReqID:     reqID,
					Status:    resp.StatusCode,
					Stream:    true,
					Body:      sseChunksToJSON(buf.Bytes()),
					ElapsedMS: time.Since(tsStart).Milliseconds(),
				})
			},
		}
	} else {
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err == nil {
			resp.Body = io.NopCloser(bytes.NewReader(body))
		}
		dumpWG.Add(1)
		go appendDump(sessID, dumpEntry{
			Type:      "response",
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Provider:  t.provider,
			ReqID:     reqID,
			Status:    resp.StatusCode,
			Body:      jsonOrRaw(body),
			ElapsedMS: time.Since(tsStart).Milliseconds(),
		})
	}

	return resp, err
}

// teeCloser reads from r (the Tee), and Close()-s the upstream c +
// flushes onClose. Mimics what httputil.DumpResponse would do with a
// streaming body.
type teeCloser struct {
	r       io.Reader
	c       io.Closer
	onClose func()
	closed  bool
}

func (t *teeCloser) Read(p []byte) (int, error) {
	return t.r.Read(p)
}

func (t *teeCloser) Close() error {
	if t.closed {
		return nil
	}
	t.closed = true
	err := t.c.Close()
	if t.onClose != nil {
		t.onClose()
	}
	return err
}

// capWriter discards bytes once `n` exceeds cap. Used inside the
// io.TeeReader pipe to bound dump file growth.
type capWriter struct {
	buf *bytes.Buffer
	cap int
}

func (c capWriter) Write(p []byte) (int, error) {
	remain := c.cap - c.buf.Len()
	if remain <= 0 {
		return len(p), nil
	}
	if len(p) > remain {
		_, _ = c.buf.Write(p[:remain])
		return len(p), nil
	}
	return c.buf.Write(p)
}

// isSSE returns true for streaming responses we want to chunk-parse.
func isSSE(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	ct := resp.Header.Get("Content-Type")
	return strings.Contains(ct, "text/event-stream")
}

// safeURL strips the API key from the URL string. claude-code uses
// path-only (`req.URL.Path`); we keep host so users can tell which
// gateway a dump went to (helpful when debugging proxy/region
// mismatches), but redact query strings entirely.
func safeURL(req *http.Request) string {
	u := *req.URL
	u.RawQuery = ""
	u.User = nil
	return u.String()
}

// redactHeaders returns a copy of h with auth-bearing values replaced
// by "[REDACTED]". The user might paste this into a bug report.
func redactHeaders(h http.Header) map[string]any {
	out := make(map[string]any, len(h))
	for k, v := range h {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "x-api-key" || lk == "anthropic-api-key" || lk == "api-key" || strings.Contains(lk, "token") || strings.Contains(lk, "secret") {
			out[k] = "[REDACTED]"
			continue
		}
		if len(v) == 1 {
			out[k] = v[0]
		} else {
			out[k] = v
		}
	}
	return out
}

// jsonOrRaw returns the body as parsed JSON (so jq users get a
// pretty AST) or as a string fallback when parsing fails.
func jsonOrRaw(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	// Validate JSON cheaply — Decoder.Decode is faster than
	// Unmarshal for this purpose.
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	// Wrap as a quoted string so the line is still valid JSONL.
	encoded, _ := json.Marshal(string(b))
	return json.RawMessage(encoded)
}

// sseChunksToJSON parses an SSE byte stream into [{event:..., data:...}]
// for replay-friendly storage. Errors during parse fall back to the
// raw bytes wrapped in a string.
func sseChunksToJSON(raw []byte) json.RawMessage {
	type chunk struct {
		Event string          `json:"event,omitempty"`
		Data  json.RawMessage `json:"data,omitempty"`
	}
	var chunks []chunk
	for _, ev := range bytes.Split(raw, []byte("\n\n")) {
		var event string
		var data []byte
		for _, line := range bytes.Split(ev, []byte("\n")) {
			switch {
			case bytes.HasPrefix(line, []byte("event:")):
				event = string(bytes.TrimSpace(line[len("event:"):]))
			case bytes.HasPrefix(line, []byte("data:")):
				data = append(data, bytes.TrimSpace(line[len("data:"):])...)
			}
		}
		if len(data) == 0 && event == "" {
			continue
		}
		// `data: [DONE]` is OpenAI's stream terminator — store as a
		// string, not JSON, since [DONE] isn't valid JSON.
		var raw json.RawMessage
		if json.Valid(data) {
			raw = json.RawMessage(data)
		} else {
			s, _ := json.Marshal(string(data))
			raw = json.RawMessage(s)
		}
		chunks = append(chunks, chunk{Event: event, Data: raw})
	}
	out, err := json.Marshal(chunks)
	if err != nil {
		s, _ := json.Marshal(string(raw))
		return json.RawMessage(s)
	}
	return json.RawMessage(out)
}

// isDumpEnabled checks env on every request — same toggleability
// rationale as isDebugEnabled.
func isDumpEnabled() bool {
	return isTruthy(os.Getenv("METIS_DUMP_PROMPTS")) || isTruthy(os.Getenv("DUMP_PROMPTS"))
}

var (
	dumpMu      sync.Mutex
	dumpHandles = map[string]*os.File{} // sessID → file handle
	// dumpWG counts pending appendDump goroutines so a process-exit
	// hook can wait for in-flight writes before terminating. Without
	// this, fast-completing `metis run` invocations exit before the
	// async goroutines flush — observed empirically as 138/304
	// missing response entries in the 300-round e2e harness.
	dumpWG sync.WaitGroup
)

// FlushDumps blocks until every in-flight appendDump goroutine has
// completed, then closes the open file handles. Called from
// cmd/metis/main.go's shutdown path so a `metis run` exit doesn't
// drop pending response-body writes mid-flush.
//
// Idempotent — safe to call multiple times. After the first call,
// subsequent appendDump invocations still work (they re-open files
// lazily via dumpHandles), so a concurrent late-firing goroutine
// won't crash; the timing window in metis run is narrow because no
// new HTTP traffic is initiated after the agent loop exits.
func FlushDumps() {
	dumpWG.Wait()
	dumpMu.Lock()
	defer dumpMu.Unlock()
	for k, f := range dumpHandles {
		_ = f.Close()
		delete(dumpHandles, k)
	}
}

// appendDump opens-or-reuses a per-session file under
// ~/.metis/dump-prompts/<sid>.jsonl and appends one entry. Best-effort
// — failures are silent (we never want logging to crash the agent).
func appendDump(sessID string, e dumpEntry) {
	defer dumpWG.Done()
	if sessID == "" {
		sessID = "default"
	}
	dumpMu.Lock()
	defer dumpMu.Unlock()
	f, ok := dumpHandles[sessID]
	if !ok {
		dir := filepath.Join(metisHome(), "dump-prompts")
		_ = os.MkdirAll(dir, 0o700)
		path := filepath.Join(dir, sessID+".jsonl")
		fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		dumpHandles[sessID] = fh
		f = fh
	}
	enc, err := json.Marshal(e)
	if err != nil {
		return
	}
	_, _ = f.Write(enc)
	_, _ = f.Write([]byte("\n"))
}

// closeDumpForTest flushes + closes all open handles. Test-only.
func closeDumpForTest() {
	dumpMu.Lock()
	defer dumpMu.Unlock()
	for k, f := range dumpHandles {
		_ = f.Close()
		delete(dumpHandles, k)
	}
}

// _ = httputil.DumpRequest // import-keeper for future raw-mode
var _ = httputil.DumpRequest

// dumpAuthFor lets register.go / Wrap callers stitch an auth token
// suffix back if needed. Currently unused; placeholder to keep the
// import block honest if we later want bearer-rewriting.
var _ = fmt.Sprintf
