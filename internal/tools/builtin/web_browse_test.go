package builtin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/security"
)

// TestWebBrowse_FallbackToHTTPWhenNoChromium — when no chromium binary
// is present (CI containers, minimal servers), the tool degrades to a
// plain HTTP fetch and clearly labels the output. We force the
// fallback path by using a URL the test server controls.
func TestWebBrowse_FallbackToHTTPWhenNoChromium(t *testing.T) {
	previousPicker := chromiumBinaryForExecution
	chromiumBinaryForExecution = func() string { return "" }
	t.Cleanup(func() { chromiumBinaryForExecution = previousPicker })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body>hello world</body></html>"))
	}))
	defer srv.Close()

	wb := WebBrowse{gate: bypassGate()}
	res, err := wb.Execute(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Errorf("expected success; got error: %s", res.Output)
	}
	// We can't guarantee chromium absence on every CI, but if the
	// fallback fired the output starts with [fallback: ...]. Either
	// way, "hello world" must appear.
	if !strings.Contains(res.Output, "hello world") {
		t.Errorf("output missing rendered text; got:\n%s", res.Output)
	}
}

func TestWebBrowseChromiumArgsDoNotUseHostKeychainOnMacOS(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("macOS-specific Chrome credential behavior")
	}
	// Keep this assertion beside the hermetic fallback test: removing the flag
	// would make any future real-browser integration test wake the user's
	// keychain dialog again.
	args := appendPlatformChromeIsolationArgs(nil)
	if !strings.Contains(strings.Join(args, " "), "--use-mock-keychain") {
		t.Fatal("macOS headless Chrome must use a mock keychain")
	}
}

// TestWebBrowse_RejectsNonHTTPURL — file://, javascript:, etc. are
// blocked at the URL-validation layer, never reach the browser.
func TestWebBrowse_RejectsNonHTTPURL(t *testing.T) {
	wb := WebBrowse{gate: bypassGate()}
	res, _ := wb.Execute(context.Background(), map[string]any{"url": "file:///etc/passwd"})
	if !res.IsError || !strings.Contains(res.Output, "url must be absolute http") {
		t.Errorf("file:// should be rejected; got %+v", res)
	}
	res, _ = wb.Execute(context.Background(), map[string]any{"url": ""})
	if !res.IsError || !strings.Contains(res.Output, "url is required") {
		t.Errorf("empty URL should be rejected; got %+v", res)
	}
}

// TestStripBoilerplate — remove <script> and <style> blocks so the
// model isn't paying tokens for minified JS bundles.
func TestStripBoilerplate(t *testing.T) {
	in := `<html>
<head>
<style>body{color:red}</style>
</head>
<body>
<script>console.log("noise")</script>
hello
<script src="x.js"></script>
</body>
</html>`
	out := stripBoilerplate(in)
	if strings.Contains(out, "console.log") || strings.Contains(out, "color:red") {
		t.Errorf("script/style not stripped; got:\n%s", out)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("body content lost; got:\n%s", out)
	}
}

// TestPickChromiumBinary — best-effort: just verify the function
// doesn't crash and returns a string (empty or path).
func TestPickChromiumBinary(t *testing.T) {
	got := pickChromiumBinary()
	t.Logf("pickChromiumBinary() = %q (empty is fine on CI)", got)
	// No assertion — environment-dependent.
}

func TestPlainFetchBlocksMetadataAndRFC1918(t *testing.T) {
	for _, rawURL := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/admin",
		"http://192.168.1.5/admin",
	} {
		_, err := plainFetch(context.Background(), rawURL)
		if !errors.Is(err, security.ErrBlocked) {
			t.Errorf("plainFetch(%q) error = %v, want SSRF block", rawURL, err)
		}
	}
}

func TestPlainFetchRevalidatesRedirectDestination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	_, err := plainFetch(context.Background(), srv.URL)
	if !errors.Is(err, security.ErrBlocked) {
		t.Fatalf("redirect to metadata must be blocked, got %v", err)
	}
}

func TestGuardedBrowserProxyBlocksMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxy, err := startGuardedBrowserProxy(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Close()

	proxyURL, err := url.Parse(proxy.URL())
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: transport,
	}
	resp, err := client.Get("http://169.254.169.254/latest/meta-data/")
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("metadata status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestGuardedBrowserProxyRevalidatesRedirectDestination(t *testing.T) {
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer redirector.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxy, err := startGuardedBrowserProxy(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL())
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport}
	resp, err := client.Get(redirector.URL)
	if err != nil {
		t.Fatalf("redirect through proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("redirected metadata status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestGuardedBrowserProxyBlocksMetadataConnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxy, err := startGuardedBrowserProxy(ctx)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Close()

	conn, err := net.DialTimeout("tcp", proxy.listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "CONNECT 169.254.169.254:443 HTTP/1.1\r\nHost: 169.254.169.254:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("metadata CONNECT status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestLimitWebBrowseOutputCapsBytes(t *testing.T) {
	in := strings.Repeat("x", maxWebBrowseOutputBytes+4096)
	got := limitWebBrowseOutput(in)
	if len(got) > maxWebBrowseOutputBytes {
		t.Fatalf("output len = %d, want <= %d", len(got), maxWebBrowseOutputBytes)
	}
	if !strings.Contains(got, "WebBrowse output truncated") {
		t.Fatal("truncation marker missing")
	}
}
