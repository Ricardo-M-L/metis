package builtin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// ddgLiteFixture is a minimal DDG lite HTML response with two
// results. The class names + the `//duckduckgo.com/l/?uddg=` redirect
// format match what live DDG returns as of 2026-05.
const ddgLiteFixture = `<!DOCTYPE html>
<html><body>
<table>
  <tr>
    <td>1.</td>
    <td>
      <a class="result-link" href="//duckduckgo.com/l/?uddg=https%3A//example.com/foo&amp;rut=abc">First result title</a>
    </td>
  </tr>
  <tr>
    <td class="result-snippet">First result snippet text.</td>
  </tr>
  <tr>
    <td>2.</td>
    <td>
      <a class="result-link" href="https://direct.example.org/bar">Second result title</a>
    </td>
  </tr>
  <tr>
    <td class="result-snippet">Second snippet.</td>
  </tr>
</table>
</body></html>`

func TestParseDDGLite_ExtractsTitleLinkSnippet(t *testing.T) {
	results, err := parseDDGLite(ddgLiteFixture, 10)
	if err != nil {
		t.Fatalf("parseDDGLite: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].Title != "First result title" {
		t.Errorf("title[0]: %q", results[0].Title)
	}
	if results[0].Link != "https://example.com/foo" {
		t.Errorf("link[0] should be unwrapped from DDG redirect; got %q", results[0].Link)
	}
	if results[0].Snippet != "First result snippet text." {
		t.Errorf("snippet[0]: %q", results[0].Snippet)
	}
	if results[1].Link != "https://direct.example.org/bar" {
		t.Errorf("link[1]: %q", results[1].Link)
	}
	if results[0].Position != 1 || results[1].Position != 2 {
		t.Errorf("positions: %d, %d", results[0].Position, results[1].Position)
	}
}

func TestParseDDGLite_RespectsMaxResults(t *testing.T) {
	results, err := parseDDGLite(ddgLiteFixture, 1)
	if err != nil {
		t.Fatalf("parseDDGLite: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result with max=1; got %d", len(results))
	}
}

func TestCleanDDGRedirect(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			"//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Ffoo%3Fa%3Db&rut=xx",
			"https://example.com/foo?a=b",
		},
		{"https://plain.example/path", "https://plain.example/path"},
		{"//duckduckgo.com/l/?uddg=", ""},
	}
	for _, tc := range cases {
		if got := cleanDDGRedirect(tc.in); got != tc.want {
			t.Errorf("cleanDDGRedirect(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSearchDuckDuckGo_Integration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "metis" {
			t.Errorf("query passthrough failed: %q", r.URL.Query().Get("q"))
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(ddgLiteFixture))
	}))
	defer srv.Close()

	// Override the URL by going through searchDuckDuckGo with a tiny
	// reachability tweak: we can't change the const URL, but we CAN
	// route via http.Client with a custom Transport that maps the DDG
	// host onto the test server.
	client := &http.Client{Transport: &rewriteTransport{
		base:     http.DefaultTransport,
		fromHost: "lite.duckduckgo.com",
		toURL:    srv.URL,
	}}
	results, err := searchDuckDuckGo(context.Background(), client, "metis", 10)
	if err != nil {
		t.Fatalf("searchDuckDuckGo: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
}

func TestWebSearch_EmptyQueryIsSoftError(t *testing.T) {
	gate := permission.New(permission.ModeBypass)
	ws := WebSearch{gate: gate}
	res, err := ws.Execute(context.Background(), map[string]any{"query": ""})
	if err != nil {
		t.Fatalf("hard error not expected: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("empty query should soft-fail, got %+v", res)
	}
}

func TestWebSearch_NameIsClaudeCodeAligned(t *testing.T) {
	got := (WebSearch{}).Name()
	if got != "WebSearch" {
		t.Fatalf("WebSearch.Name() = %q; want \"WebSearch\" (claude-code parity)", got)
	}
}

func TestFormatSearchResults_NoResults(t *testing.T) {
	got := formatSearchResults("xyz", nil)
	if !strings.Contains(got, "no results") {
		t.Errorf("empty-results output should explain to LLM; got %q", got)
	}
}

func TestFormatSearchResults_FormatsAllFields(t *testing.T) {
	got := formatSearchResults("test", []webSearchResult{
		{Title: "Foo", Link: "https://foo.example", Snippet: "About foo.", Position: 1},
	})
	for _, want := range []string{"test", "Foo", "https://foo.example", "About foo."} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

// rewriteTransport rewrites outbound requests targeting fromHost to
// land on toURL instead. Used to redirect lite.duckduckgo.com →
// httptest server for the integration test without changing
// production code.
type rewriteTransport struct {
	base     http.RoundTripper
	fromHost string
	toURL    string
}

func (rt *rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == rt.fromHost {
		newReq := r.Clone(r.Context())
		// Strip scheme prefix (httptest URL is "http://127.0.0.1:NNN").
		if i := strings.Index(rt.toURL, "://"); i != -1 {
			newReq.URL.Scheme = rt.toURL[:i]
			newReq.URL.Host = rt.toURL[i+3:]
		}
		return rt.base.RoundTrip(newReq)
	}
	return rt.base.RoundTrip(r)
}
