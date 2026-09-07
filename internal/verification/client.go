// Package verification is the narrow host-side client for a preregistered local
// browser broker. It neither executes model-provided commands nor imports agent.
package verification

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/security"
)

const maxResponseBytes = 1 << 20

var (
	tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)
	portPattern  = regexp.MustCompile(`^[0-9]+$`)
	idPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	jobPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// Client is immutable after construction and safe for concurrent use. Only
// trusted host setup supplies its endpoint and token; never expose either as
// model tool arguments. Run's total budget comes from its caller's context.
type Client struct {
	endpoint     string
	token        string
	http         *http.Client
	pollInterval time.Duration
}

// Protect incidental diagnostic formatting of the client itself.
func (Client) String() string   { return "verification.Client{credentials:[REDACTED]}" }
func (Client) GoString() string { return "verification.Client{credentials:[REDACTED]}" }

type Check struct {
	ID             string          `json:"id"`
	Status         string          `json:"status"` // passed, failed, not_tested, error
	ExitCode       *int            `json:"exit_code"`
	DurationMS     *float64        `json:"duration_ms"` // Preserve null and fractional milliseconds.
	ArtifactSHA256 string          `json:"artifact_sha256"`
	Details        json.RawMessage `json:"details"`
}

type Evidence struct {
	SourceSHA256 string  `json:"source_sha256"`
	Checks       []Check `json:"checks"`
}

type Result struct {
	Checks []Check `json:"checks"`
}

type JobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Job struct {
	JobID         string    `json:"job_id"`
	Suite         string    `json:"suite"`
	Status        string    `json:"status"` // pending, running, completed, failed, cancelled
	SourceSHA256  string    `json:"source_sha256"`
	SourceChanged bool      `json:"source_changed"`
	Result        Result    `json:"result"`
	Error         *JobError `json:"error"`
}

func NewClient(rawURL, token string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawPath != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(rawURL, "#") ||
		(u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") || !portPattern.MatchString(u.Port()) {
		return nil, errors.New("verification broker requires an HTTP loopback literal and explicit numeric port, without URL extras")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || u.Host != net.JoinHostPort(u.Hostname(), u.Port()) {
		return nil, errors.New("verification broker port is invalid")
	}
	if !tokenPattern.MatchString(token) {
		return nil, errors.New("verification broker token must be 32 to 256 URL-safe characters")
	}
	transport := &http.Transport{
		Proxy: nil, // Never consult HTTP_PROXY, ALL_PROXY, PAC, or host credentials.
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
		}).DialContext,
		DisableKeepAlives:      true, // No reused-connection automatic replay of ambiguous POSTs.
		ResponseHeaderTimeout:  10 * time.Second,
		MaxResponseHeaderBytes: 32 << 10,
	}
	return &Client{endpoint: u.String(), token: token, pollInterval: 500 * time.Millisecond,
		http: &http.Client{Transport: transport, Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect forbidden") }}}, nil
}

func (c *Client) SourceHash(ctx context.Context) (string, error) {
	var reply struct {
		SourceSHA256 string `json:"source_sha256"`
	}
	if err := c.request(ctx, http.MethodGet, "/v1/source", nil, &reply); err != nil {
		return "", err
	}
	if !validHash(reply.SourceSHA256) || strings.Contains(reply.SourceSHA256, c.token) {
		return "", errors.New("verification broker returned an invalid source hash")
	}
	return reply.SourceSHA256, nil
}

func (c *Client) Acceptance(ctx context.Context) (Evidence, error) {
	var evidence Evidence
	if err := c.request(ctx, http.MethodGet, "/v1/acceptance", nil, &evidence); err != nil {
		return Evidence{}, err
	}
	if !validHash(evidence.SourceSHA256) || validateChecks(evidence.Checks) != nil {
		return Evidence{}, errors.New("verification broker returned invalid acceptance evidence")
	}
	return c.redactEvidence(evidence), nil
}

// Run submits exactly once, then polls a bounded job ID. Completed describes
// execution, not passing checks; failed/cancelled jobs return their redacted DTO
// with nil error. Transport/protocol failures remain errors, never a false pass.
// Context cancellation best-effort cancels the known job with an independent
// three-second context. Ambiguous start failure is never retried.
func (c *Client) Run(ctx context.Context, suite string) (Job, error) {
	if !validSuite(suite) {
		return Job{}, errors.New("verification suite must be smoke, controls, or endurance")
	}
	var job Job
	if err := c.request(ctx, http.MethodPost, "/v1/jobs", struct {
		Suite string `json:"suite"`
	}{suite}, &job); err != nil {
		if jobPattern.MatchString(job.JobID) && job.Suite == suite {
			c.cancel(ctx, job.JobID)
		}
		return Job{}, err
	}
	knownID := job.JobID
	for {
		if err := validateJob(job, suite, knownID); err != nil {
			if jobPattern.MatchString(knownID) {
				c.cancel(ctx, knownID)
			}
			return Job{}, err
		}
		if err := ctx.Err(); err != nil {
			c.cancel(ctx, knownID)
			return c.redactJob(job), err
		}
		if job.Status == "completed" || job.Status == "failed" || job.Status == "cancelled" {
			return c.redactJob(job), nil
		}
		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			c.cancel(ctx, knownID)
			return c.redactJob(job), ctx.Err()
		case <-timer.C:
		}
		var next Job
		if err := c.request(ctx, http.MethodGet, "/v1/jobs/"+knownID, nil, &next); err != nil {
			c.cancel(ctx, knownID)
			return c.redactJob(job), err
		}
		job = next
	}
}

func (c *Client) cancel(parent context.Context, jobID string) {
	ctx, done := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
	defer done()
	var discarded Job
	_ = c.request(ctx, http.MethodPost, "/v1/jobs/"+jobID+"/cancel", struct{}{}, &discarded)
}

func (c *Client) request(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return errors.New("verification request could not be encoded")
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, reader)
	if err != nil {
		return errors.New("verification request could not be created")
	}
	req.GetBody = nil // Also prohibit transparent body replay; no idempotency headers.
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("verification broker request failed") // Never echo URL errors, body, or token.
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("verification broker returned an unsuccessful HTTP status")
	}
	if response.ContentLength > maxResponseBytes {
		return errors.New("verification broker response exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("verification broker response could not be read")
	}
	if len(data) > maxResponseBytes {
		return errors.New("verification broker response exceeds size limit")
	}
	if err := strictJSON(data, out); err != nil {
		return errors.New("verification broker returned invalid JSON")
	}
	if err := validateWireShape(data, out); err != nil {
		return errors.New("verification broker returned an incomplete response shape")
	}
	return nil
}

func validSuite(suite string) bool {
	return suite == "smoke" || suite == "controls" || suite == "endurance"
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateJob(job Job, suite, jobID string) error {
	if !jobPattern.MatchString(job.JobID) || job.JobID != jobID || job.Suite != suite ||
		(job.SourceSHA256 != "" && !validHash(job.SourceSHA256)) || validateChecks(job.Result.Checks) != nil {
		return errors.New("verification broker returned an invalid or mismatched job")
	}
	switch job.Status {
	case "pending", "running", "completed", "failed", "cancelled":
		return nil
	default:
		return errors.New("verification broker returned an unknown job state")
	}
}

func validateChecks(checks []Check) error {
	if len(checks) > 256 {
		return errors.New("too many checks")
	}
	seen := make(map[string]bool, len(checks))
	for _, check := range checks {
		if !idPattern.MatchString(check.ID) || seen[check.ID] ||
			(check.DurationMS != nil && (math.IsNaN(*check.DurationMS) || math.IsInf(*check.DurationMS, 0) || *check.DurationMS < 0 || *check.DurationMS >= math.MaxInt64)) ||
			(check.ArtifactSHA256 != "" && !validHash(check.ArtifactSHA256)) {
			return errors.New("invalid check fields")
		}
		seen[check.ID] = true
		switch check.Status {
		case "passed", "failed", "not_tested", "error":
		default:
			return errors.New("unknown check status")
		}
	}
	return nil
}

// Decode with a separate token pass to reject duplicate keys, trailing values,
// and deeply nested responses (DisallowUnknownFields alone permits duplicates).
func strictJSON(data []byte, out any) error {
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' || !utf8.Valid(data) {
		return errors.New("expected object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := uniqueValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out)
}

// encoding/json accepts null for scalar/struct values and cannot distinguish a
// missing field from its zero value. The broker contract makes those distinctions
// explicit, especially an absent result versus an actual empty checks array.
func validateWireShape(data []byte, target any) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	required := func(object map[string]json.RawMessage, name string, nullable bool) bool {
		value, ok := object[name]
		return ok && (nullable || !bytes.Equal(bytes.TrimSpace(value), []byte("null")))
	}
	var checks json.RawMessage
	switch target.(type) {
	case *Job:
		for _, name := range []string{"job_id", "suite", "status", "source_sha256", "source_changed", "result", "error"} {
			if !required(fields, name, name == "source_sha256" || name == "error") {
				return errors.New("missing or null job field")
			}
		}
		var result map[string]json.RawMessage
		if json.Unmarshal(fields["result"], &result) != nil || !required(result, "checks", false) {
			return errors.New("missing checks")
		}
		checks = result["checks"]
		if !bytes.Equal(bytes.TrimSpace(fields["error"]), []byte("null")) {
			var fault map[string]json.RawMessage
			if json.Unmarshal(fields["error"], &fault) != nil || !required(fault, "code", false) || !required(fault, "message", false) {
				return errors.New("incomplete job error")
			}
		}
	case *Evidence:
		if !required(fields, "source_sha256", false) || !required(fields, "checks", false) {
			return errors.New("missing evidence fields")
		}
		checks = fields["checks"]
	default:
		return nil // SourceHash separately validates its sole non-null SHA256 field.
	}
	var rows []map[string]json.RawMessage
	if json.Unmarshal(checks, &rows) != nil {
		return errors.New("invalid check objects")
	}
	for _, row := range rows {
		for _, name := range []string{"id", "status", "exit_code", "duration_ms", "artifact_sha256", "details"} {
			if !required(row, name, name != "id" && name != "status") {
				return errors.New("missing check field")
			}
		}
	}
	return nil
}

func uniqueValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON nesting limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate or invalid object key")
			}
			seen[name] = true
			if err := uniqueValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := uniqueValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected delimiter")
	}
	_, err = decoder.Token()
	return err
}

func (c *Client) redact(value string) string {
	return security.RedactSubprocessText(strings.ReplaceAll(value, c.token, "[REDACTED]"))
}

func (c *Client) redactEvidence(evidence Evidence) Evidence {
	evidence.SourceSHA256 = c.redact(evidence.SourceSHA256)
	evidence.Checks = c.redactChecks(evidence.Checks)
	return evidence
}

func (c *Client) redactJob(job Job) Job {
	job.JobID, job.Suite, job.Status = c.redact(job.JobID), c.redact(job.Suite), c.redact(job.Status)
	job.SourceSHA256 = c.redact(job.SourceSHA256)
	job.Result.Checks = c.redactChecks(job.Result.Checks)
	if job.Error != nil {
		job.Error.Code, job.Error.Message = c.redact(job.Error.Code), c.redact(job.Error.Message)
	}
	return job
}

func (c *Client) redactChecks(checks []Check) []Check {
	for index := range checks {
		check := &checks[index]
		check.ID, check.Status, check.ArtifactSHA256 = c.redact(check.ID), c.redact(check.Status), c.redact(check.ArtifactSHA256)
		if len(check.Details) != 0 {
			var detail any
			decoder := json.NewDecoder(bytes.NewReader(check.Details))
			decoder.UseNumber()
			if decoder.Decode(&detail) == nil {
				check.Details, _ = json.Marshal(c.redactValue(detail))
			} else {
				check.Details = json.RawMessage(`"[REDACTED]"`)
			}
		}
	}
	return checks
}

func (c *Client) redactValue(value any) any {
	switch value := value.(type) {
	case string:
		return c.redact(value)
	case json.Number:
		if strings.Contains(value.String(), c.token) {
			return "[REDACTED]"
		}
		return value
	case []any:
		for i := range value {
			value[i] = c.redactValue(value[i])
		}
		return value
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[c.redact(key)] = c.redactValue(item)
		}
		return out
	default:
		return value
	}
}
