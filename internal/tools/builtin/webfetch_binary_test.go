package builtin

// webfetch_binary_test.go — covers the 2026-05-18 P0 fix:
// WebFetch must NOT pipe binary bodies through `string(body)`. On
// the wild bug (user image #9) a model fetched a model-generated PNG
// URL and the raw bytes blew 164k tokens of garbage into the prompt.
//
// Post-fix:
//   - image/* / video/* / audio/* / application/octet-stream / pdf
//     are saved to ~/.metis/tool-results/ and a single-line pointer
//     replaces the body in the tool_result.
//   - mislabeled responses (no Content-Type but body is not valid
//     UTF-8) get the same treatment as a defensive third layer.
//   - text/* / application/json / unknown-but-valid-utf-8 still go
//     through the legacy string-body path so HTML/JSON continue to
//     work.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// pngFixture is a real (tiny) PNG byte sequence. Magic header +
// IHDR chunk + small data + IEND. We don't need a renderable image,
// just non-UTF-8 bytes the test server can return as Content-Type
// image/png.
var pngFixture = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, // signature
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, // IHDR len + tag
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, // IEND
	0xae, 0x42, 0x60, 0x82,
}

func TestWebFetch_PNG_SavedNotInlined(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(200)
		_, _ = w.Write(pngFixture)
	}))
	defer srv.Close()

	wf := WebFetch{gate: bypassGate()}
	res, err := wf.Execute(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	// Body must NOT appear in tool output (no raw PNG bytes leak).
	if strings.ContainsRune(res.Output, '\x89') || strings.ContainsRune(res.Output, '\x00') {
		t.Errorf("raw PNG bytes leaked into tool output: %q", res.Output)
	}
	if !strings.Contains(res.Output, "image/png") {
		t.Errorf("output should mention media type; got: %q", res.Output)
	}
	if !strings.Contains(res.Output, "saved to") {
		t.Errorf("output should mention save path; got: %q", res.Output)
	}
	if !strings.Contains(res.Output, "Read tool") {
		t.Errorf("output should hint at Read tool; got: %q", res.Output)
	}
}

func TestWebFetch_PNG_SavedFileExistsAndMatches(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("METIS_HOME", tmp)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngFixture)
	}))
	defer srv.Close()

	wf := WebFetch{gate: bypassGate()}
	res, err := wf.Execute(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	// Extract path from output.
	idx := strings.Index(res.Output, "saved to ")
	if idx < 0 {
		t.Fatalf("no save path in output: %q", res.Output)
	}
	rest := res.Output[idx+len("saved to "):]
	end := strings.IndexAny(rest, "]\n")
	if end < 0 {
		t.Fatalf("malformed save path in output: %q", res.Output)
	}
	path := rest[:end]
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(body) != string(pngFixture) {
		t.Errorf("saved file bytes differ from server response")
	}
	if !strings.HasSuffix(path, ".png") {
		t.Errorf("saved file should have .png extension; got %s", path)
	}
}

func TestWebFetch_TextContent_StillInlined(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>hello</body></html>"))
	}))
	defer srv.Close()

	wf := WebFetch{gate: bypassGate()}
	res, err := wf.Execute(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("text body should appear inline; got: %q", res.Output)
	}
	if strings.Contains(res.Output, "saved to") {
		t.Errorf("text body should NOT be saved to disk; got: %q", res.Output)
	}
}

func TestWebFetch_NoContentType_NonUTF8_Suppressed(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately no Content-Type header. Body is binary garbage.
		_, _ = w.Write([]byte{0xff, 0xfe, 0xfd, 0xfc, 0x00, 0x80, 0x90, 0xa0})
	}))
	defer srv.Close()

	wf := WebFetch{gate: bypassGate()}
	res, err := wf.Execute(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	for _, b := range []byte{0xff, 0xfe, 0xfd} {
		if strings.ContainsRune(res.Output, rune(b)) {
			t.Errorf("non-UTF8 byte 0x%x leaked into output", b)
		}
	}
}

func TestWebFetch_OctetStream_SavedAsBin(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0x01, 0x02, 0x03, 0x04})
	}))
	defer srv.Close()

	wf := WebFetch{gate: bypassGate()}
	res, err := wf.Execute(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !strings.Contains(res.Output, "saved to") {
		t.Errorf("octet-stream should be saved; got: %q", res.Output)
	}
	if !strings.Contains(res.Output, "application/octet-stream") {
		t.Errorf("mime should appear in summary; got: %q", res.Output)
	}
}

func TestExtensionForBinary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mime   string
		url    string
		expect string
	}{
		{"image/png", "image/png", "https://x.com/p", ".png"},
		{"image/jpeg", "image/jpeg", "https://x.com/p", ".jpg"},
		{"url fallback", "", "https://x.com/foo.webp", ".webp"},
		{"url fallback with query", "", "https://x.com/foo.webp?v=1", ".webp"},
		{"final fallback", "", "https://x.com/path/no/extension", ".bin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extensionForBinary(c.mime, c.url)
			if got != c.expect {
				t.Errorf("extensionForBinary(%q, %q) = %q, want %q", c.mime, c.url, got, c.expect)
			}
		})
	}
}
