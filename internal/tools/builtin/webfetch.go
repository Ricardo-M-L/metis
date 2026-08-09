package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type WebFetch struct {
	tools.BaseTool
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

	var (
		status       string
		statusCode   int
		responseHead http.Header
		body         []byte
		bodyComplete bool
	)
	err := transport.RetryWithBackoff(cctx, 3, 0, func() error {
		// Clear the prior attempt before dialing. If this attempt dies before
		// headers, the final error must remain visible rather than accidentally
		// returning an older 503 body as though it were the last response.
		status = ""
		statusCode = 0
		responseHead = nil
		body = nil
		bodyComplete = false
		// A fresh request is required for each retry. GET has no body and is
		// idempotent, so replay is safe after EOF/DNS/reset/read truncation.
		req, err := http.NewRequestWithContext(cctx, "GET", url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "metis/0.1 (+local)")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		status, statusCode = resp.Status, resp.StatusCode
		responseHead = resp.Header.Clone()
		body, err = io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		bodyComplete = true
		if transport.IsRetryableStatus(resp.StatusCode) {
			httpErr := fmt.Errorf("webfetch %d: %s", resp.StatusCode, transport.Truncate(string(body), 500))
			return &transport.RetryableError{Err: httpErr, After: transport.ParseRetryAfter(resp)}
		}
		return nil
	})
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
		// A fully-read final HTTP response remains useful evidence (status +
		// provider body) even when all retryable attempts were exhausted.
		// Transport/read failures have no trustworthy body and stay errors.
		if statusCode == 0 || !bodyComplete {
			return nil, err
		}
	}
	truncatedSuffix := ""
	if len(body) >= maxBytes {
		truncatedSuffix = "\n\n[truncated at " + bytesString(maxBytes) + "]"
	}

	// Binary-content守门 (2026-05-18, user image #9 bug).
	// Pre-fix: any body went straight through `string(body)`, which
	// turned a 250KB PNG response into 250KB of UTF-8 garbage that
	// burned ~164k context tokens (caught in the wild — image_paste
	// of a model output URL). Now Content-Type drives the dispatch:
	//
	//   image/*  video/*  audio/*  application/octet-stream
	//     → write to ~/.metis/tool-results/webfetch-<ts>-<rand>.<ext>
	//       return a one-line "saved to X" pointer; model can Read it
	//       if its provider supports vision.
	//   anything else with no Content-Type but the body is not valid
	//     UTF-8 → same "binary, suppressed" path (defensive layer per
	//     crush's pattern).
	//   text/* / application/json / fallback → original string-body
	//     return path (unchanged).
	ct := responseHead.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(ct)
	mediaType = strings.ToLower(mediaType)

	isBinary := strings.HasPrefix(mediaType, "image/") ||
		strings.HasPrefix(mediaType, "video/") ||
		strings.HasPrefix(mediaType, "audio/") ||
		mediaType == "application/octet-stream" ||
		mediaType == "application/pdf"
	// Final defense: no/wrong Content-Type but body not UTF-8 → treat
	// as binary too. Stops mislabeled image servers from leaking bytes
	// to the prompt.
	if !isBinary && mediaType == "" && len(body) > 0 && !utf8.Valid(body) {
		isBinary = true
	}

	if isBinary {
		savedPath, saveErr := saveBinaryResponse(body, mediaType, url)
		hintMime := mediaType
		if hintMime == "" {
			hintMime = "application/octet-stream"
		}
		if saveErr != nil {
			// Save failed (disk full / perms). Don't poison the prompt
			// with raw bytes — surface the error cleanly.
			return &tools.Result{
				Output: fmt.Sprintf("HTTP %s\n[binary content (%s, %s) suppressed — failed to save: %v]",
					status, hintMime, bytesString(len(body)), saveErr),
				IsError: true,
			}, nil
		}
		summary := fmt.Sprintf(
			"HTTP %s\n[binary content (%s, %s) saved to %s]\n"+
				"Use the Read tool with this absolute path to view the file. "+
				"If the model has vision capability and this is an image, "+
				"Read will surface it as an image block; otherwise it will be "+
				"reported as binary.",
			status, hintMime, bytesString(len(body)), savedPath,
		)
		return &tools.Result{
			Output:  summary + truncatedSuffix,
			IsError: statusCode >= 400,
		}, nil
	}

	out := string(body)
	return &tools.Result{
		Output:  "HTTP " + status + "\n" + out + truncatedSuffix,
		IsError: statusCode >= 400,
	}, nil
}

// saveBinaryResponse writes a non-text WebFetch body to
// ~/.metis/tool-results/webfetch-<ts>-<rand>.<ext>. Extension is
// picked from the media type (image/png → .png) with a URL-suffix
// fallback when the server forgot the Content-Type. Returns the
// absolute path or an OS error.
//
// Mirrors claude-code's persistBinaryContent pattern: never inline
// binary into prompt, always hand the model a file path it can later
// Read at its discretion.
func saveBinaryResponse(body []byte, mediaType, srcURL string) (string, error) {
	dir := filepath.Join(config.Home(), "tool-results")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	ts := time.Now().Format("20060102-150405")
	ext := extensionForBinary(mediaType, srcURL)
	name := fmt.Sprintf("webfetch-%s-%s%s", ts, hex.EncodeToString(nonce[:]), ext)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// preferredExtensions overrides mime.ExtensionsByType for the cases
// where the stdlib returns a non-canonical extension. Go's mime db
// for image/jpeg returns `.jpe` first (alphabetical); humans expect
// `.jpg`. Same for image/svg+xml (stdlib has no entry on macOS).
var preferredExtensions = map[string]string{
	"image/jpeg":      ".jpg",
	"image/svg+xml":   ".svg",
	"image/x-icon":    ".ico",
	"audio/mpeg":      ".mp3",
	"video/quicktime": ".mov",
	"text/markdown":   ".md",
	"application/zip": ".zip",
}

// extensionForBinary picks a file extension for the saved binary.
// Priority:
//  1. preferredExtensions map (curated common cases)
//  2. mime.ExtensionsByType (Go stdlib mapping)
//  3. URL path suffix (last `.xxx` segment, when reasonable)
//  4. `.bin` fallback
func extensionForBinary(mediaType, srcURL string) string {
	if mediaType != "" {
		if ext, ok := preferredExtensions[mediaType]; ok {
			return ext
		}
		if exts, err := mime.ExtensionsByType(mediaType); err == nil && len(exts) > 0 {
			// Prefer the shortest / most common (".jpg" over ".jpeg" etc).
			best := exts[0]
			for _, e := range exts {
				if len(e) < len(best) {
					best = e
				}
			}
			return best
		}
	}
	// URL-suffix fallback: take the last `.xxx` segment whose
	// position is close to the end of the URL (within ~10 chars of
	// EOF), then strip any query/fragment.
	if idx := strings.LastIndex(srcURL, "."); idx >= 0 && len(srcURL)-idx <= 10 {
		suffix := srcURL[idx:]
		if q := strings.IndexAny(suffix, "?#&"); q >= 0 {
			suffix = suffix[:q]
		}
		if len(suffix) >= 2 && len(suffix) <= 6 {
			return strings.ToLower(suffix)
		}
	}
	return ".bin"
}
