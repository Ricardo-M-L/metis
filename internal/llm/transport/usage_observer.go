package transport

// This observer is deliberately a transport boundary, rather than a loop
// event listener. It records physical attempts (including provider retries
// and non-streaming helpers) before request dispatch and before usage-bearing
// response bytes are released to a provider parser.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

type RequestMetadata struct {
	ClientRequestID string `json:"client_request_id"`
	Provider        string `json:"provider"`
	EndpointSHA256  string `json:"endpoint_sha256"`
	Model           string `json:"model"`
	RequestSHA256   string `json:"request_sha256"`
}

// UsageSnapshot is absolute reported usage for one physical request. Input
// excludes cached input. Unreported fields retain the previous snapshot.
type UsageSnapshot struct {
	ResponseID               string `json:"response_id,omitempty"`
	UsageReported            bool   `json:"usage_reported"`
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
}

type UsageStatus string

const (
	UsageCompleted   UsageStatus = "completed"
	UsageInterrupted UsageStatus = "interrupted"
	UsageRejected    UsageStatus = "rejected"
)

type UsageObserver interface {
	Begin(context.Context, RequestMetadata) (RequestUsageObserver, error)
}
type RequestUsageObserver interface {
	Header(requestID string) error
	Snapshot(UsageSnapshot) error
	End(UsageStatus) error
}

// UsageHTTPStatusObserver is optional for older observer implementations.
// Only the integer status is exposed; headers and response bodies stay private.
type UsageHTTPStatusObserver interface{ HTTPStatus(int) error }

type usageObserverContextKey struct{}

// UsageObserverFromContext lets an execution-bound provider retain the same
// durable ledger authority when a helper uses a detached caller context.
// The context key stays private so arbitrary callers cannot impersonate it.
func UsageObserverFromContext(ctx context.Context) UsageObserver {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(usageObserverContextKey{}).(UsageObserver)
	return observer
}

func WithUsageObserver(ctx context.Context, observer UsageObserver) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, usageObserverContextKey{}, observer)
}

// HasUsageObserver identifies contexts attached to the durable transport
// ledger. Budget callers must reconcile this ledger with their legacy totals
// rather than adding two representations of the same usage together.
func HasUsageObserver(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	observer, _ := ctx.Value(usageObserverContextKey{}).(UsageObserver)
	return observer != nil
}

// UsageObserverError intentionally does not unwrap its callback cause. A disk
// error can itself wrap EOF or net.Error; provider retry must not mistake a
// failed durable write for a retryable model network failure. Callback text
// can contain file paths or secrets and is never put in the model error.
type UsageObserverError struct {
	Stage string
	Code  string
	Err   error
}

func (e *UsageObserverError) Error() string {
	if e.Code == "provider_usage_unresolved" {
		return "durable model usage observer failed at " + e.Stage + ": provider_usage_unresolved"
	}
	return "durable model usage observer failed at " + e.Stage
}

// SafeUsageObserverClassification preserves the callback boundary for host
// diagnostics without exposing its arbitrary cause or creating an unwrap path
// to EOF/network errors. Providers use this copy inside their redacted errors.
func SafeUsageObserverClassification(err error) error {
	var original *UsageObserverError
	if !errors.As(err, &original) || original == nil {
		return nil
	}
	stage := "unknown"
	switch original.Stage {
	case "unsupported_protocol", "request_metadata", "request_identity", "begin", "response_headers", "response_identity", "response_status", "end", "response_bound", "response_metadata", "response_closed", "snapshot", "usage_counter":
		stage = original.Stage
	}
	code := ""
	if original.Code == "provider_usage_unresolved" {
		code = original.Code
	}
	return &UsageObserverError{Stage: stage, Code: code}
}

const usageRequestLimit = 32 << 20
const usageResponseLimit = 32 << 20
const usageFrameLimit = 4 << 20
const usageIdentityLimit = 512

func usageFailure(stage string, err error) error {
	code := ""
	var coded interface{ UsageFailureCode() string }
	if errors.As(err, &coded) && coded.UsageFailureCode() == "provider_usage_unresolved" {
		code = "provider_usage_unresolved"
	}
	return &UsageObserverError{Stage: stage, Code: code, Err: err}
}

func validUsageIdentity(id string) bool {
	if len(id) > usageIdentityLimit {
		return false
	}
	for _, c := range id {
		if c <= 32 || c == 127 {
			return false
		}
	}
	return true
}

func beginRequestUsage(req *http.Request, provider string) (*http.Request, *observedUsageBody, error) {
	observer, _ := req.Context().Value(usageObserverContextKey{}).(UsageObserver)
	if observer == nil || req.Method != http.MethodPost {
		return req, nil, nil
	}
	if provider != "anthropic" && provider != "openai" && provider != "openai-responses" {
		// Unknown wire protocols cannot silently bypass usage accounting while
		// the loop relies on this observer as the sole budget writer.
		return nil, nil, usageFailure("unsupported_protocol", errors.New("provider usage protocol is unsupported"))
	}
	if req.GetBody == nil {
		return nil, nil, usageFailure("request_metadata", errors.New("request body is not replayable"))
	}
	copyBody, err := req.GetBody()
	if err != nil {
		return nil, nil, usageFailure("request_metadata", err)
	}
	raw, err := io.ReadAll(io.LimitReader(copyBody, usageRequestLimit+1))
	closeErr := copyBody.Close()
	if err != nil || closeErr != nil || len(raw) > usageRequestLimit {
		return nil, nil, usageFailure("request_metadata", errors.New("request metadata cannot be read within its bound"))
	}
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil, nil, usageFailure("request_metadata", errors.New("invalid request JSON"))
	}
	if body.Model == "" || !validUsageIdentity(body.Model) {
		return nil, nil, usageFailure("request_metadata", errors.New("model identity unavailable"))
	}
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return nil, nil, usageFailure("request_identity", err)
	}
	meta := RequestMetadata{ClientRequestID: hex.EncodeToString(idBytes[:]), Provider: provider, Model: body.Model}
	// URL credentials, query and fragment are deliberately excluded. Only the
	// fingerprint is persisted, never even the canonical endpoint string.
	endpoint := strings.ToLower(req.URL.Scheme) + "://" + strings.ToLower(req.URL.Host) + req.URL.EscapedPath()
	endpointHash, requestHash := sha256.Sum256([]byte(endpoint)), sha256.Sum256(raw)
	meta.EndpointSHA256, meta.RequestSHA256 = hex.EncodeToString(endpointHash[:]), hex.EncodeToString(requestHash[:])
	requestObserver, err := observer.Begin(req.Context(), meta)
	if err != nil {
		return nil, nil, usageFailure("begin", err)
	}
	if requestObserver == nil {
		return nil, nil, usageFailure("begin", errors.New("nil request observer"))
	}
	clone := req.Clone(req.Context())
	clone.Header.Set(HeaderRequestID, meta.ClientRequestID)
	// Gateways may assign a separate request ID without returning it in HTTP
	// headers. A non-secret nonce in their recorded User-Agent provides an exact
	// join to this pre-dispatch ledger identity, including after a worker crash.
	var agentTokens []string
	for _, token := range strings.Fields(req.Header.Get("User-Agent")) {
		if !strings.HasPrefix(token, "metis-usage/") {
			agentTokens = append(agentTokens, token)
		}
	}
	if len(agentTokens) == 0 {
		agentTokens = append(agentTokens, "Go-http-client/1.1")
	}
	agentTokens = append(agentTokens, "metis-usage/"+meta.ClientRequestID)
	clone.Header.Set("User-Agent", strings.Join(agentTokens, " "))
	state := &observedUsageBody{observer: requestObserver, stream: body.Stream, protocol: provider}
	return clone, state, nil
}

func (b *observedUsageBody) attach(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return usageFailure("response_headers", errors.New("missing response body"))
	}
	if status, ok := b.observer.(UsageHTTPStatusObserver); ok {
		if resp.StatusCode < 100 || resp.StatusCode > 599 {
			return usageFailure("response_status", errors.New("invalid HTTP response status"))
		}
		if err := status.HTTPStatus(resp.StatusCode); err != nil {
			return usageFailure("response_status", err)
		}
	}
	for _, name := range []string{"request-id", "x-request-id"} {
		if id := resp.Header.Get(name); id != "" {
			if !validUsageIdentity(id) {
				return usageFailure("response_identity", errors.New("invalid request identity"))
			}
			if err := b.observer.Header(id); err != nil {
				return usageFailure("response_headers", err)
			}
			break
		}
	}
	if resp.StatusCode >= 400 {
		return b.finish(UsageRejected)
	}
	b.inner = resp.Body
	b.stream = b.stream || strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	if b.stream {
		b.reader = bufio.NewReaderSize(resp.Body, 4096)
	}
	resp.Body = b
	return nil
}

type observedUsageBody struct {
	observer                RequestUsageObserver
	inner                   io.ReadCloser
	reader                  *bufio.Reader
	stream                  bool
	protocol                string
	snapshot                UsageSnapshot
	inputTotal, cachedTotal int64
	hasInputTotal           bool
	ready                   []byte
	pendingErr              error
	loaded                  bool
	mu                      sync.Mutex // serializes Read/Close callbacks, not the blocking source read
	ended                   bool
	closed                  bool
}

func (b *observedUsageBody) finish(status UsageStatus) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ended {
		return nil
	}
	if err := b.observer.End(status); err != nil {
		return usageFailure("end", err)
	}
	b.ended = true
	return nil
}

func (b *observedUsageBody) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.mu.Unlock()
	// Close the source first to unblock a concurrent read; no mutex is held
	// during the network read or its cancellation.
	closeErr := b.inner.Close()
	if err := b.finish(UsageInterrupted); err != nil {
		return err
	}
	return closeErr
}

func (b *observedUsageBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.ready) > 0 {
		n := copy(p, b.ready)
		b.ready = b.ready[n:]
		return n, nil
	}
	if b.pendingErr != nil {
		return 0, b.pendingErr
	}
	var raw []byte
	var err error
	if b.stream {
		raw, err = b.readFrame()
		if len(raw) > 0 {
			if observeErr := b.observeFrame(raw); observeErr != nil {
				return b.failRead(observeErr)
			}
		}
	} else {
		if b.loaded {
			return 0, io.EOF
		}
		b.loaded = true
		raw, err = io.ReadAll(io.LimitReader(b.inner, usageResponseLimit+1))
		if len(raw) > usageResponseLimit {
			return b.failRead(usageFailure("response_bound", errors.New("response exceeds bound")))
		}
		if err == nil {
			if observeErr := b.observeJSON(raw, true); observeErr != nil {
				return b.failRead(observeErr)
			}
			err = io.EOF
		}
	}
	if err != nil {
		if finishErr := b.finish(UsageInterrupted); finishErr != nil {
			return b.failRead(finishErr)
		}
		b.pendingErr = err
	}
	if len(raw) == 0 {
		return 0, err
	}
	b.ready = raw
	n := copy(p, b.ready)
	b.ready = b.ready[n:]
	return n, nil
}

func (b *observedUsageBody) failRead(err error) (int, error) {
	_ = b.inner.Close()
	if finishErr := b.finish(UsageInterrupted); finishErr != nil {
		err = errors.Join(err, finishErr)
	}
	b.ready = nil
	b.pendingErr = err
	return 0, err
}

func (b *observedUsageBody) readFrame() ([]byte, error) {
	var frame []byte
	for {
		line, err := b.reader.ReadSlice('\n')
		if len(frame)+len(line) > usageFrameLimit {
			return nil, usageFailure("response_bound", errors.New("SSE frame exceeds bound"))
		}
		frame = append(frame, line...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return frame, err
		}
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			return frame, nil
		}
	}
}

func (b *observedUsageBody) observeFrame(frame []byte) error {
	var data []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimPrefix(line[5:], []byte(" "))
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
	}
	if len(data) == 0 {
		return nil
	}
	if string(data) == "[DONE]" {
		return b.finish(UsageCompleted)
	}
	return b.observeJSON(data, false)
}

type reportedUsage struct {
	Input         *int64 `json:"input_tokens"`
	Output        *int64 `json:"output_tokens"`
	Prompt        *int64 `json:"prompt_tokens"`
	Completion    *int64 `json:"completion_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
	CacheCreate   *int64 `json:"cache_creation_input_tokens"`
	PromptDetails *struct {
		Cached *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputDetails *struct {
		Cached *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	PromptCacheHit *int64 `json:"prompt_cache_hit_tokens"`
	Cached         *int64 `json:"cached_tokens"`
}

func (b *observedUsageBody) observeJSON(raw []byte, complete bool) error {
	var obj struct {
		Type    string         `json:"type"`
		ID      string         `json:"id"`
		Status  string         `json:"status"`
		Usage   *reportedUsage `json:"usage"`
		Message *struct {
			ID    string         `json:"id"`
			Usage *reportedUsage `json:"usage"`
		} `json:"message"`
		Response *struct {
			ID     string         `json:"id"`
			Status string         `json:"status"`
			Usage  *reportedUsage `json:"usage"`
		} `json:"response"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return usageFailure("response_metadata", errors.New("invalid response JSON"))
	}
	usage, id := obj.Usage, obj.ID
	if obj.Message != nil {
		id = obj.Message.ID
		if obj.Message.Usage != nil {
			usage = obj.Message.Usage
		}
	}
	if obj.Response != nil {
		id = obj.Response.ID
		if obj.Response.Usage != nil {
			usage = obj.Response.Usage
		}
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return usageFailure("response_closed", errors.New("response is closed"))
	}
	previous := b.snapshot
	if id != "" {
		if !validUsageIdentity(id) || previous.ResponseID != "" && previous.ResponseID != id {
			b.mu.Unlock()
			return usageFailure("response_identity", errors.New("conflicting response identity"))
		}
		b.snapshot.ResponseID = id
	}
	if usage != nil {
		if usage.Input != nil || usage.Output != nil || usage.Prompt != nil || usage.Completion != nil || usage.CacheRead != nil || usage.CacheCreate != nil || usage.Cached != nil || usage.PromptCacheHit != nil || usage.PromptDetails != nil && usage.PromptDetails.Cached != nil || usage.InputDetails != nil && usage.InputDetails.Cached != nil {
			b.snapshot.UsageReported = true
		}
		if err := b.mergeUsage(usage, obj.Response != nil || strings.Contains(b.protocol, "openai") || usage.Prompt != nil || usage.InputDetails != nil); err != nil {
			b.mu.Unlock()
			return err
		}
	}
	if b.snapshot != previous {
		if err := b.observer.Snapshot(b.snapshot); err != nil {
			b.mu.Unlock()
			return usageFailure("snapshot", err)
		}
	}
	b.mu.Unlock()
	if obj.Type == "error" || obj.Type == "response.failed" || obj.Type == "response.cancelled" || len(obj.Error) > 0 && string(obj.Error) != "null" {
		return b.finish(UsageInterrupted)
	}
	if complete {
		if id == "" && usage == nil {
			// A syntactically valid JSON body is not itself a model completion
			// marker (for example an empty gateway object or JSON null).
			return b.finish(UsageInterrupted)
		}
		if obj.Status != "" && obj.Status != "completed" && obj.Status != "incomplete" {
			return b.finish(UsageInterrupted)
		}
		return b.finish(UsageCompleted)
	}
	if obj.Type == "message_stop" || obj.Type == "response.completed" || obj.Type == "response.incomplete" || obj.Type == "response.done" {
		if obj.Response != nil && obj.Response.Status != "" && obj.Response.Status != "completed" && obj.Response.Status != "incomplete" {
			return b.finish(UsageInterrupted)
		}
		return b.finish(UsageCompleted)
	}
	return nil
}

func (b *observedUsageBody) mergeUsage(u *reportedUsage, openAI bool) error {
	assign := func(dst *int64, value *int64) error {
		if value == nil {
			return nil
		}
		if *value < 0 {
			return usageFailure("usage_counter", errors.New("negative token counter"))
		}
		*dst = *value
		return nil
	}
	if err := assign(&b.snapshot.OutputTokens, u.Output); err != nil {
		return err
	}
	if err := assign(&b.snapshot.OutputTokens, u.Completion); err != nil {
		return err
	}
	if err := assign(&b.snapshot.CacheCreationInputTokens, u.CacheCreate); err != nil {
		return err
	}
	if !openAI {
		if err := assign(&b.snapshot.InputTokens, u.Input); err != nil {
			return err
		}
		return assign(&b.snapshot.CacheReadInputTokens, u.CacheRead)
	}
	if u.Input != nil {
		b.hasInputTotal = true
		if err := assign(&b.inputTotal, u.Input); err != nil {
			return err
		}
	}
	if u.Prompt != nil {
		b.hasInputTotal = true
		if err := assign(&b.inputTotal, u.Prompt); err != nil {
			return err
		}
	}
	// Match built-in OpenAI cacheReadTokens: nested positive counters take
	// precedence, then DeepSeek/GLM's prompt_cache_hit_tokens and cached_tokens.
	var cached *int64
	for _, candidate := range []*int64{u.CacheRead, u.Cached, u.PromptCacheHit} {
		if candidate != nil && (cached == nil || *candidate > 0) {
			cached = candidate
		}
	}
	if u.PromptDetails != nil && u.PromptDetails.Cached != nil && (cached == nil || *u.PromptDetails.Cached > 0) {
		cached = u.PromptDetails.Cached
	}
	if u.InputDetails != nil && u.InputDetails.Cached != nil && (cached == nil || *u.InputDetails.Cached > 0) {
		cached = u.InputDetails.Cached
	}
	if err := assign(&b.cachedTotal, cached); err != nil {
		return err
	}
	b.snapshot.CacheReadInputTokens = b.cachedTotal
	if b.hasInputTotal {
		if b.cachedTotal > b.inputTotal {
			return usageFailure("usage_counter", errors.New("cached tokens exceed input total"))
		}
		b.snapshot.InputTokens = b.inputTotal - b.cachedTotal
		b.snapshot.CacheReadInputTokens = b.cachedTotal
	}
	return nil
}
