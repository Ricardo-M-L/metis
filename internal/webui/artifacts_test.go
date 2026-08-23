package webui

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/artifact"
)

func TestSanitizeArtifactHTMLMakesStaticDocument(t *testing.T) {
	source := []byte(`<!doctype html>
<html><head>
<base href="https://attacker.invalid/"><meta http-equiv="refresh" content="0;url=https://attacker.invalid">
<style>@import "https://attacker.invalid/a.css"; .hero{background:url(https://attacker.invalid/x);color:red}</style>
<script>globalThis.pwned = true</script>
</head><body onload="pwn()">
<custom-card data-secret="x"><h1 style="background:url(https://attacker.invalid/y)">Visible</h1></custom-card>
<img id="remote" src="https://attacker.invalid/pixel" onerror="pwn()">
<img id="inline" src="data:image/png;base64,AAAA" alt="safe">
<a id="remote-link" href="https://attacker.invalid/">remote</a>
<a id="local-link" href="#section">local</a>
<iframe src="https://attacker.invalid/"></iframe><svg onload="pwn()"><script>pwn()</script></svg>
</body></html>`)

	got, err := sanitizeArtifactHTML(source)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, forbidden := range []string{
		"<script", "<iframe", "<svg", "<base", "<meta", "onload=", "onerror=",
		"https://attacker.invalid", "@import", "data-secret", "<custom-card",
	} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Errorf("sanitized document retained %q:\n%s", forbidden, text)
		}
	}
	for _, want := range []string{"Visible", "data:image/png;base64,AAAA", `href="#section"`, "color:red"} {
		if !strings.Contains(text, want) {
			t.Errorf("sanitized document lost %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `id="remote" src=`) {
		t.Errorf("remote image src survived sanitization:\n%s", text)
	}
	if strings.Contains(text, `id="remote-link" href=`) {
		t.Errorf("remote link href survived sanitization:\n%s", text)
	}
}

func TestArtifactPreviewUsesCapabilityOriginAndSecurityHeaders(t *testing.T) {
	preview, err := startArtifactPreview([]byte(`<h1 onclick="pwn()">Preview</h1><script>pwn()</script>`), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}

	resp, err := client.Get(preview.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("Preview")) {
		t.Fatalf("GET preview = %d %q", resp.StatusCode, body)
	}
	if bytes.Contains(body, []byte("<script")) || bytes.Contains(body, []byte("onclick")) {
		t.Fatalf("GET preview returned active HTML: %s", body)
	}
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store, max-age=0",
	} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"script-src 'none'", "connect-src 'none'", "object-src 'none'", "form-action 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q: %s", want, csp)
		}
	}
	if got := resp.Header.Get("Permissions-Policy"); !strings.Contains(got, "camera=()") || !strings.Contains(got, "microphone=()") {
		t.Errorf("Permissions-Policy is incomplete: %q", got)
	}

	req, err := http.NewRequest(http.MethodHead, preview.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("HEAD preview = %d body=%q", resp.StatusCode, body)
	}

	req, _ = http.NewRequest(http.MethodPost, preview.URL, strings.NewReader("ignored"))
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST preview = %d Allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}

	wrong, _ := url.Parse(preview.URL)
	wrong.Path = "/wrong-capability"
	resp, err = client.Get(wrong.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong capability = %d, want 404", resp.StatusCode)
	}
}

func TestArtifactPreviewRejectsWrongHostAndExpires(t *testing.T) {
	preview, err := startArtifactPreview([]byte(`<p>short lived</p>`), 40*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	req, _ := http.NewRequest(http.MethodGet, preview.URL, nil)
	req.Host = "localhost.invalid"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong Host = %d, want 404", resp.StatusCode)
	}

	select {
	case <-preview.done:
	case <-time.After(2 * time.Second):
		t.Fatal("preview listener did not close after TTL")
	}
	if _, err := client.Get(preview.URL); err == nil {
		t.Fatal("expired capability listener still accepted a connection")
	}
}

func TestArtifactPreviewRejectsInvalidInputs(t *testing.T) {
	if _, err := startArtifactPreview(nil, time.Second); err == nil {
		t.Fatal("empty HTML should fail")
	}
	if _, err := startArtifactPreview([]byte("<p>x</p>"), 0); err == nil {
		t.Fatal("zero TTL should fail")
	}
	if _, err := sanitizeArtifactHTML(bytes.Repeat([]byte("x"), maxArtifactPreviewBytes+1)); err == nil {
		t.Fatal("oversized HTML should fail")
	}
}

func TestArtifactAPIListsPreviewsDownloadsExportsAndDeletes(t *testing.T) {
	store, err := artifact.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create("session-a", "Release dashboard", `<h1>v1</h1><script>alert(1)</script>`)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.Update("session-a", created.ID, "", `<h1>v2</h1>`)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := store.Create("session-b", "Private", `<p>other session</p>`)
	if err != nil {
		t.Fatal(err)
	}

	server, _ := testServer(t)
	server.artifactStore = store
	server.activeSessionID = "session-a"
	handler := server.handler()

	do := func(method, path string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
		return recorder
	}

	list := do(http.MethodGet, "/api/artifacts?sessionId=session-a")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), created.ID) || strings.Contains(list.Body.String(), foreign.ID) {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
	detail := do(http.MethodGet, "/api/artifacts/"+created.ID)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"current_version":2`) {
		t.Fatalf("detail = %d %s", detail.Code, detail.Body.String())
	}
	conflict := do(http.MethodGet, "/api/artifacts?sessionId=session-b")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("inactive-session list = %d, want 409", conflict.Code)
	}
	forbidden := do(http.MethodGet, "/api/artifacts/"+foreign.ID)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("cross-session detail = %d, want 403", forbidden.Code)
	}

	previewResponse := do(http.MethodGet, "/api/artifacts/"+created.ID+"/preview?version=2")
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("preview API = %d %s", previewResponse.Code, previewResponse.Body.String())
	}
	var previewPayload struct {
		URL     string `json:"url"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &previewPayload); err != nil {
		t.Fatal(err)
	}
	if previewPayload.Version != 2 || !strings.HasPrefix(previewPayload.URL, "http://127.0.0.1:") {
		t.Fatalf("preview payload = %+v", previewPayload)
	}
	preview, err := (&http.Client{Timeout: time.Second}).Get(previewPayload.URL)
	if err != nil {
		t.Fatal(err)
	}
	previewBody, err := io.ReadAll(preview.Body)
	_ = preview.Body.Close()
	if err != nil || preview.StatusCode != http.StatusOK || !bytes.Contains(previewBody, []byte("v2")) {
		t.Fatalf("preview document = %d %q err=%v", preview.StatusCode, previewBody, err)
	}

	download := do(http.MethodGet, "/api/artifacts/"+created.ID+"/download?version=1")
	if download.Code != http.StatusOK || !strings.Contains(download.Header().Get("Content-Disposition"), ".html") ||
		!bytes.Contains(download.Body.Bytes(), []byte("v1")) || bytes.Contains(bytes.ToLower(download.Body.Bytes()), []byte("<script")) {
		t.Fatalf("download = %d headers=%v body=%q", download.Code, download.Header(), download.Body.Bytes())
	}

	exported := do(http.MethodGet, "/api/artifacts/"+created.ID+"/export?version=2")
	if exported.Code != http.StatusOK || exported.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("export = %d headers=%v body=%q", exported.Code, exported.Header(), exported.Body.Bytes())
	}
	archive, err := zip.NewReader(bytes.NewReader(exported.Body.Bytes()), int64(exported.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]bool{}
	for _, entry := range archive.File {
		entries[entry.Name] = true
	}
	if !entries["index.html"] || !entries["manifest.json"] {
		t.Fatalf("ZIP entries = %v", entries)
	}

	deleted := do(http.MethodDelete, "/api/artifacts/"+created.ID)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", deleted.Code, deleted.Body.String())
	}
	if _, err := store.Get("session-a", updated.ID); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("artifact survived DELETE: %v", err)
	}
}
