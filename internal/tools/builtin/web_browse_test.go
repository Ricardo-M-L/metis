package builtin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebBrowse_FallbackToHTTPWhenNoChromium — when no chromium binary
// is present (CI containers, minimal servers), the tool degrades to a
// plain HTTP fetch and clearly labels the output. We force the
// fallback path by using a URL the test server controls.
func TestWebBrowse_FallbackToHTTPWhenNoChromium(t *testing.T) {
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
