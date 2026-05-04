// Package builtin — WebBrowse tool.
//
// claude-code's WEB_BROWSER_TOOL drives a real browser to scrape pages
// that need JavaScript execution (SPA routes, infinite scroll, content
// rendered by client-side hydration). WebFetch alone returns the raw
// HTML, which is empty for these.
//
// metis WebBrowse: shell out to a headless chromium / chrome / edge
// process and capture the rendered DOM as text. Falls back to plain
// HTTP fetch when no browser binary is available — degrades cleanly
// rather than failing in low-tooling environments (CI containers,
// minimal Linux servers).
//
// Detection order: chromium, chrome, google-chrome, microsoft-edge,
// brave-browser. First found wins.
package builtin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// WebBrowse renders a URL via headless chromium and returns extracted
// page text. Subject to the same permission gate as WebFetch.
type WebBrowse struct{ gate *permission.Gate }

func NewWebBrowse(gate *permission.Gate) WebBrowse { return WebBrowse{gate: gate} }

func (WebBrowse) Name() string { return "WebBrowse" }

func (WebBrowse) Description() string {
	return "Render a URL via headless chromium and return the extracted page text. " +
		"Use for JS-heavy sites where WebFetch returns empty content (SPAs, infinite-scroll, etc). " +
		"Falls back to plain HTTP fetch when no headless browser is installed."
}

func (WebBrowse) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"url"},
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "Absolute URL to render (https://...)",
			},
			"wait_ms": map[string]any{
				"type":        "integer",
				"description": "Optional render wait in ms after page load (default 800). Use larger values for SPAs that fetch async content.",
			},
		},
	}
}

func (WebBrowse) Concurrency(map[string]any) tools.Concurrency {
	// Spawning chromium is heavy — serialize so we don't fork 5
	// concurrent browser processes. Queue tier (rate-limited serial)
	// matches what WebFetch uses.
	return tools.ConcurrencyQueue
}

func (w WebBrowse) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	u, _ := in["url"].(string)
	d, src := w.gate.Check(context.Background(), "WebBrowse", u)
	return mapDecision(d), src
}

func (WebBrowse) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	rawURL, _ := in["url"].(string)
	if rawURL == "" {
		return &tools.Result{Output: "error: url is required", IsError: true}, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return &tools.Result{Output: "error: url must be absolute http(s)", IsError: true}, nil
	}
	wait := 800
	if w, ok := in["wait_ms"].(float64); ok && w > 0 && w < 30000 {
		wait = int(w)
	}

	binary := pickChromiumBinary()
	if binary == "" {
		// Fallback: plain HTTP fetch. Honest about the degradation in
		// the result so the model knows it didn't get JS-rendered DOM.
		body, ferr := plainFetch(ctx, rawURL)
		if ferr != nil {
			return &tools.Result{
				Output:  fmt.Sprintf("error: no headless chromium installed AND fallback HTTP fetch failed: %v", ferr),
				IsError: true,
			}, nil
		}
		return &tools.Result{
			Output: "[fallback: no headless browser installed; this is plain HTTP fetch, no JS executed]\n\n" + body,
		}, nil
	}

	// Chromium --headless --dump-dom prints the post-JS DOM to stdout.
	// --virtual-time-budget gives the page <wait>ms to settle async work
	// (matches puppeteer's default behavior).
	cmd := exec.CommandContext(ctx, binary,
		"--headless=new",
		"--disable-gpu",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--hide-scrollbars",
		fmt.Sprintf("--virtual-time-budget=%d", wait),
		"--dump-dom",
		rawURL,
	)
	// Hard-cap total runtime so a stuck page doesn't hang the agent.
	ctx2, cancel := context.WithTimeout(ctx, time.Duration(wait+15000)*time.Millisecond)
	defer cancel()
	cmd = exec.CommandContext(ctx2, cmd.Path, cmd.Args[1:]...)
	out, err := cmd.Output()
	if err != nil {
		return &tools.Result{
			Output:  fmt.Sprintf("error: chromium exec failed: %v", err),
			IsError: true,
		}, nil
	}
	// Strip script/style blocks since the model wants visible text, not
	// minified bundles. Crude regex-free extraction; good enough for
	// the LLM to pick out signal.
	text := stripBoilerplate(string(out))
	if text == "" {
		return &tools.Result{Output: "[browser returned empty content — page may have crashed or required login]"}, nil
	}
	return &tools.Result{Output: text}, nil
}

// pickChromiumBinary returns the absolute path to the first installed
// chromium-family browser. Empty string when none found — caller must
// handle the fallback path.
func pickChromiumBinary() string {
	candidates := []string{
		"chromium",
		"chromium-browser",
		"google-chrome",
		"chrome",
		"microsoft-edge",
		"brave-browser",
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	// Common macOS install paths that aren't in PATH.
	if _, err := os.Stat("/Applications/Chromium.app/Contents/MacOS/Chromium"); err == nil {
		return "/Applications/Chromium.app/Contents/MacOS/Chromium"
	}
	if _, err := os.Stat("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"); err == nil {
		return "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	}
	return ""
}

// plainFetch is the fallback when no headless browser is available.
// Same surface as the existing WebFetch tool but inlined here so we
// don't take a runtime dependency on it.
func plainFetch(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "metis-WebBrowse/1.0 (fallback HTTP)")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024)) // 1 MB cap
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// stripBoilerplate is a tiny pre-processor that removes <script> and
// <style> blocks so the returned text isn't 90% minified bundle. Not a
// full HTML parser — the model handles whatever residual tags remain.
func stripBoilerplate(s string) string {
	for _, tag := range []string{"script", "style"} {
		open := "<" + tag
		close := "</" + tag + ">"
		for {
			i := strings.Index(strings.ToLower(s), open)
			if i < 0 {
				break
			}
			end := strings.Index(strings.ToLower(s[i:]), close)
			if end < 0 {
				break
			}
			s = s[:i] + s[i+end+len(close):]
		}
	}
	return strings.TrimSpace(s)
}
