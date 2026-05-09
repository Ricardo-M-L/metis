package builtin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type WebFetch struct {
	gate *permission.Gate
	http *http.Client
}

func (WebFetch) Name() string { return "WebFetch" }
func (WebFetch) Description() string {
	return "Fetch a URL and return the response body (truncated). HTML is returned as-is; caller is expected to extract relevant info."
}
func (WebFetch) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"url"},
		"properties": map[string]any{
			"url":        map[string]any{"type": "string"},
			"max_bytes":  map[string]any{"type": "integer"},
			"timeout_ms": map[string]any{"type": "integer"},
		},
	}
}

// WebFetch is concurrency-safe: claude-code, openclaude, and hermes all
// classify their equivalents (WebFetchTool / web_extract) as parallel-OK.
// Different URLs typically hit different domains, the Go http client
// pools connections, and rate-limit handling belongs at the HTTP layer
// (Retry-After) not the tool dispatcher. Forcing FIFO would 3x the
// wall-time when the model asks for three fetches at once for no real
// safety win.
func (WebFetch) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (w WebFetch) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := w.gate.Check(context.Background(), "WebFetch", strFromAny(in["url"]))
	return mapDecision(d), src
}

func (w WebFetch) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	url, _ := in["url"].(string)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, errors.New("url must start with http:// or https://")
	}
	maxBytes := intArg(in, "max_bytes", 256*1024)
	timeoutMs := intArg(in, "timeout_ms", 15000)

	client := w.http
	if client == nil {
		// Default client wires the SSRF guard into the dialer (see
		// internal/security/ssrf.go) so model-controlled URLs can't
		// reach RFC1918 / link-local / cloud-metadata IPs. Loopback
		// (127.0.0.0/8, ::1) stays allowed for local dev. Tests that
		// pass their own w.http skip the guard intentionally.
		client = &http.Client{
			Timeout: time.Duration(timeoutMs) * time.Millisecond,
			Transport: &http.Transport{
				DialContext: security.GuardedDialContext,
			},
		}
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "metis/0.1 (+local)")
	resp, err := client.Do(req)
	if err != nil {
		// Clean up the SSRF block message — the wrapped DNSError /
		// net.OpError makes it noisy. errors.Is unwraps to ErrBlocked.
		if errors.Is(err, security.ErrBlocked) {
			var be *security.BlockedError
			if errors.As(err, &be) {
				return &tools.Result{
					Output:  fmt.Sprintf("WebFetch blocked: %s", be.Error()),
					IsError: true,
				}, nil
			}
		}
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
	out := string(body)
	if len(body) >= maxBytes {
		out += "\n\n[truncated at " + bytesString(maxBytes) + "]"
	}
	return &tools.Result{
		Output:  "HTTP " + resp.Status + "\n" + out,
		IsError: resp.StatusCode >= 400,
	}, nil
}
