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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// WebBrowse renders a URL via headless chromium and returns extracted
// page text. Subject to the same permission gate as WebFetch.
//
// IsEnabled is self-aware (2026-05-15): WebBrowse hides itself when no
// chromium-family binary is on disk. Reason — without a real browser
// WebBrowse can only fall back to plain HTTP fetch, which is exactly
// what WebFetch already does; exposing two tools with identical
// behavior just bloats the model's decision matrix. The fallback path
// below stays as defensive code (binary uninstalled mid-session) but
// is no longer a primary advertised feature.
type WebBrowse struct{ gate *permission.Gate }

func NewWebBrowse(gate *permission.Gate) WebBrowse { return WebBrowse{gate: gate} }

func (WebBrowse) Name() string { return "WebBrowse" }

// IsEnabled gates WebBrowse on the presence of a headless-capable
// chromium binary. Computed once per process via sync.Once so the
// 6+ exec.LookPath / os.Stat probes in pickChromiumBinary don't run
// per registration check. Mirrors claude-code's WebBrowserTool gate
// (tools.ts:217 — `...(WebBrowserTool ? [WebBrowserTool] : [])`).
func (WebBrowse) IsEnabled() bool { return cachedChromiumBinary() != "" }

func (WebBrowse) Description() string {
	return "Render a known absolute URL via headless Chromium and return the extracted page text. " +
		"Use only after WebFetch is insufficient because the page requires JavaScript " +
		"(SPAs, client-side hydration, infinite scroll, etc.); not for keyword search."
}

func (WebBrowse) SearchHint() string { return "browse javascript known url after webfetch" }

func (WebBrowse) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"url"},
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "Known absolute http(s) URL to render after WebFetch proves insufficient.",
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

	binary := chromiumBinaryForExecution()
	if binary == "" {
		// Fallback: plain HTTP fetch. Honest about the degradation in
		// the result so the model knows it didn't get JS-rendered DOM.
		body, ferr := plainFetch(ctx, rawURL)
		if ferr != nil {
			return &tools.Result{
				Output: limitWebBrowseOutput(fmt.Sprintf(
					"error: no headless chromium installed AND fallback HTTP fetch failed: %s",
					security.RedactSubprocessText(ferr.Error()))),
				IsError: true,
			}, nil
		}
		return &tools.Result{
			Output: limitWebBrowseOutput(
				"[fallback: no headless browser installed; this is plain HTTP fetch, no JS executed]\n\n" +
					security.RedactSubprocessText(body)),
		}, nil
	}

	// The browser gets an ephemeral home/profile and a guarded loopback-only
	// proxy. This keeps host cookies and profiles out of the child while making
	// every network connection (main document, redirects, subresources and
	// HTTPS CONNECT tunnels) pass through security.GuardedDialContext.
	tmpHome, err := os.MkdirTemp("", "metis-webbrowse-")
	if err != nil {
		return guardedWebBrowseFallback(ctx, rawURL, "could not create isolated browser profile: "+err.Error())
	}
	defer os.RemoveAll(tmpHome)
	if err := os.Chmod(tmpHome, 0o700); err != nil {
		return guardedWebBrowseFallback(ctx, rawURL, "could not secure isolated browser profile: "+err.Error())
	}
	profileDir := filepath.Join(tmpHome, "profile")
	cacheDir := filepath.Join(tmpHome, "cache")
	configDir := filepath.Join(tmpHome, "config")
	for _, dir := range []string{profileDir, cacheDir, configDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return guardedWebBrowseFallback(ctx, rawURL, "could not initialize isolated browser profile: "+err.Error())
		}
	}

	// Hard-cap total runtime so a stuck page doesn't hang the agent. The same
	// context owns the proxy, tunnels and browser process.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(wait+15000)*time.Millisecond)
	proxy, err := startGuardedBrowserProxy(runCtx)
	if err != nil {
		cancel()
		return guardedWebBrowseFallback(ctx, rawURL, "could not start guarded browser proxy: "+err.Error())
	}
	defer func() {
		// Cancel in-flight proxy requests before waiting for the server to close.
		cancel()
		_ = proxy.Close()
	}()

	// Chromium --headless --dump-dom prints the post-JS DOM to stdout.
	// --virtual-time-budget gives the page <wait>ms to settle async work
	// (matches puppeteer's default behavior).
	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-extensions",
		"--disable-sync",
		"--disable-quic",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1",
		"--proxy-server=" + proxy.URL(),
		"--proxy-bypass-list=<-loopback>",
		"--no-first-run",
		"--no-default-browser-check",
		"--incognito",
		"--hide-scrollbars",
		"--user-data-dir=" + profileDir,
		"--disk-cache-dir=" + cacheDir,
	}
	// An ephemeral profile does not stop Chrome on macOS from consulting the
	// named "Chrome" Safe Storage keychain. Headless WebBrowse must never wake
	// a host credential prompt, especially during automated tests.
	args = appendPlatformChromeIsolationArgs(args)
	args = append(args,
		fmt.Sprintf("--virtual-time-budget=%d", wait),
		"--dump-dom",
		rawURL,
	)
	cmd := exec.CommandContext(runCtx, binary, args...)
	cmd.Env = browserSubprocessEnv(tmpHome, cacheDir, configDir)
	var stdout, stderr cappedWebBrowseBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	configureWebBrowseProcess(cmd)
	err = runWebBrowseCommand(cmd)
	if err != nil {
		// An installed browser is not necessarily runnable in the current
		// environment (CI sandboxes, low-memory hosts, damaged installations).
		// Preserve WebBrowse's documented graceful degradation instead of
		// failing a URL that the plain HTTP path can still retrieve.
		// The browser budget may be exhausted (which is a normal reason for
		// CommandContext to fail). Use the caller context for the independent,
		// guarded HTTP fallback so an expired browser-only deadline does not make
		// graceful degradation impossible. plainFetch has its own 15 second cap.
		body, fallbackErr := plainFetch(ctx, rawURL)
		if fallbackErr == nil {
			return &tools.Result{
				Output: limitWebBrowseOutput(fmt.Sprintf(
					"[fallback: chromium failed (%s); this is plain HTTP fetch, no JS executed]\n\n%s",
					browserExecutionError(err, stderr.String()), security.RedactSubprocessText(body))),
			}, nil
		}
		return &tools.Result{
			Output: limitWebBrowseOutput(fmt.Sprintf(
				"error: chromium exec failed: %s; fallback HTTP fetch failed: %s",
				browserExecutionError(err, stderr.String()),
				security.RedactSubprocessText(fallbackErr.Error()))),
			IsError: true,
		}, nil
	}
	// Strip script/style blocks since the model wants visible text, not
	// minified bundles. Crude regex-free extraction; good enough for
	// the LLM to pick out signal.
	text := stripBoilerplate(stdout.String())
	if text == "" {
		return &tools.Result{Output: "[browser returned empty content — page may have crashed or required login]"}, nil
	}
	return &tools.Result{Output: limitWebBrowseOutput(security.RedactSubprocessText(text))}, nil
}

func guardedWebBrowseFallback(ctx context.Context, rawURL, reason string) (*tools.Result, error) {
	body, err := plainFetch(ctx, rawURL)
	if err == nil {
		return &tools.Result{Output: limitWebBrowseOutput(fmt.Sprintf(
			"[fallback: %s; this is plain HTTP fetch, no JS executed]\n\n%s",
			security.RedactSubprocessText(reason), security.RedactSubprocessText(body)))}, nil
	}
	return &tools.Result{
		Output: limitWebBrowseOutput(fmt.Sprintf("error: %s; guarded fallback HTTP fetch failed: %s",
			security.RedactSubprocessText(reason), security.RedactSubprocessText(err.Error()))),
		IsError: true,
	}, nil
}

func browserExecutionError(err error, stderr string) string {
	message := security.RedactSubprocessText(err.Error())
	stderr = strings.TrimSpace(security.RedactSubprocessText(stderr))
	if stderr != "" {
		message += ": " + stderr
	}
	return limitWebBrowseOutput(message)
}

func browserSubprocessEnv(home, cacheDir, configDir string) []string {
	env := security.RestrictedSubprocessEnv(os.Environ(),
		"HOME="+home,
		"XDG_CACHE_HOME="+cacheDir,
		"XDG_CONFIG_HOME="+configDir,
		"XDG_DATA_HOME="+filepath.Join(home, "data"),
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
	)
	// Chromium is forced through our guarded loopback proxy via command-line
	// flags. Remove ambient proxy variables (especially NO_PROXY) so they cannot
	// change that routing decision.
	filtered := env[:0]
	for _, binding := range env {
		name, _, _ := strings.Cut(binding, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			continue
		}
		filtered = append(filtered, binding)
	}
	return filtered
}

// pickChromiumBinary returns the absolute path to the first installed
// chromium-family browser. Empty string when none found — caller must
// handle the fallback path.
//
// This is the uncached implementation used at Execute time (the user
// might install / uninstall chromium between sessions, and the
// per-call cost — 6 LookPath + 2 Stat — is negligible compared to
// the chromium spawn itself). The IsEnabled hot path uses
// cachedChromiumBinary instead so registry-time filtering is O(1).
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

// chromiumBinaryForExecution is a narrow test seam. Unit tests that exercise
// the HTTP fallback must force the empty result instead of launching whatever
// browser happens to be installed on the developer workstation.
var chromiumBinaryForExecution = pickChromiumBinary

func appendPlatformChromeIsolationArgs(args []string) []string {
	if goruntime.GOOS == "darwin" {
		return append(args, "--use-mock-keychain")
	}
	return args
}

// chromiumBinaryCache memoizes the first pickChromiumBinary call so
// IsEnabled doesn't pay the probe cost on every registration check.
// Bound for life-of-process; a user installing chromium mid-session
// would need to restart metis to see WebBrowse appear — acceptable
// trade for not stat'ing 8 paths per filter pass.
var (
	chromiumOnce   sync.Once
	chromiumCached string
)

// cachedChromiumBinary is the IsEnabled-friendly accessor. Computed
// once per process from pickChromiumBinary.
func cachedChromiumBinary() string {
	chromiumOnce.Do(func() {
		chromiumCached = pickChromiumBinary()
	})
	return chromiumCached
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
	transport := newGuardedBrowserTransport()
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if req.URL == nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
				return errors.New("redirect target must use http(s)")
			}
			// The redirected request uses the same guarded transport. Its
			// destination is resolved and validated again immediately before dial.
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWebBrowseOutputBytes+1))
	if err != nil {
		return "", err
	}
	return limitWebBrowseOutput(string(body)), nil
}

const (
	maxWebBrowseOutputBytes  = 1 << 20
	maxBrowserProxyBodyBytes = 16 << 20
	browserProxyTunnelTTL    = 2 * time.Minute
)

type cappedWebBrowseBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *cappedWebBrowseBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxWebBrowseOutputBytes - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return n, nil
}

func (b *cappedWebBrowseBuffer) String() string {
	out := b.buf.String()
	if b.truncated {
		out += "\n[WebBrowse output truncated]\n"
	}
	return limitWebBrowseOutput(out)
}

func limitWebBrowseOutput(text string) string {
	if len(text) <= maxWebBrowseOutputBytes {
		return text
	}
	const marker = "\n[WebBrowse output truncated]\n"
	limit := maxWebBrowseOutputBytes - len(marker)
	text = text[:limit]
	for len(text) > 0 && !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text + marker
}

func newGuardedBrowserTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           security.GuardedDialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
	}
}

type guardedBrowserProxy struct {
	server    *http.Server
	listener  net.Listener
	handler   *guardedBrowserProxyHandler
	closed    chan struct{}
	closeOnce sync.Once
}

func startGuardedBrowserProxy(ctx context.Context) (*guardedBrowserProxy, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !tcpAddr.IP.IsLoopback() {
		_ = listener.Close()
		return nil, errors.New("guarded browser proxy did not bind loopback")
	}
	handler := &guardedBrowserProxyHandler{
		ctx:       ctx,
		transport: newGuardedBrowserTransport(),
		tunnels:   make(map[net.Conn]struct{}),
	}
	proxy := &guardedBrowserProxy{
		listener: listener,
		handler:  handler,
		closed:   make(chan struct{}),
	}
	proxy.server = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() {
		_ = proxy.server.Serve(listener)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = proxy.Close()
		case <-proxy.closed:
		}
	}()
	return proxy, nil
}

func (p *guardedBrowserProxy) URL() string {
	return "http://" + p.listener.Addr().String()
}

func (p *guardedBrowserProxy) Close() error {
	var closeErr error
	p.closeOnce.Do(func() {
		close(p.closed)
		p.handler.close()
		closeErr = p.server.Close()
		_ = p.listener.Close()
	})
	return closeErr
}

type guardedBrowserProxyHandler struct {
	ctx       context.Context
	transport *http.Transport
	mu        sync.Mutex
	tunnels   map[net.Conn]struct{}
}

func (p *guardedBrowserProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.serveConnect(w, r)
		return
	}
	if r.URL == nil || r.URL.Host == "" || (r.URL.Scheme != "http" && r.URL.Scheme != "https") {
		http.Error(w, "invalid proxy target", http.StatusBadRequest)
		return
	}
	out := r.Clone(p.ctx)
	out.RequestURI = ""
	out.Host = out.URL.Host
	out.Header = r.Header.Clone()
	removeHopByHopHeaders(out.Header)
	out.Header.Del("Proxy-Authorization")
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, security.ErrBlocked) {
			status = http.StatusForbidden
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	defer resp.Body.Close()
	removeHopByHopHeaders(resp.Header)
	resp.Header.Del("Content-Length")
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxBrowserProxyBodyBytes))
}

func (p *guardedBrowserProxyHandler) serveConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if target == "" {
		target = r.URL.Host
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		if strings.Contains(err.Error(), "missing port in address") {
			target = net.JoinHostPort(target, "443")
		} else {
			http.Error(w, "invalid CONNECT target", http.StatusBadRequest)
			return
		}
	}
	upstream, err := security.GuardedDialContext(p.ctx, "tcp", target)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, security.ErrBlocked) {
			status = http.StatusForbidden
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "proxy tunnel unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	deadline := time.Now().Add(browserProxyTunnelTTL)
	_ = client.SetDeadline(deadline)
	_ = upstream.SetDeadline(deadline)
	p.trackTunnel(client, true)
	p.trackTunnel(upstream, true)
	defer func() {
		p.trackTunnel(client, false)
		p.trackTunnel(upstream, false)
		_ = client.Close()
		_ = upstream.Close()
	}()
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, buffered)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
	_ = client.Close()
	_ = upstream.Close()
	<-done
}

func (p *guardedBrowserProxyHandler) trackTunnel(conn net.Conn, add bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if add {
		p.tunnels[conn] = struct{}{}
	} else {
		delete(p.tunnels, conn)
	}
}

func (p *guardedBrowserProxyHandler) close() {
	p.transport.CloseIdleConnections()
	p.mu.Lock()
	defer p.mu.Unlock()
	for conn := range p.tunnels {
		_ = conn.Close()
	}
}

func removeHopByHopHeaders(header http.Header) {
	for _, token := range strings.Split(header.Get("Connection"), ",") {
		if token = strings.TrimSpace(token); token != "" {
			header.Del(token)
		}
	}
	for _, name := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		header.Del(name)
	}
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
