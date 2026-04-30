package skills

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGitHubSearch returns a canned API payload that looks like real github
// /search/code output so we can pin parsing without hitting the network.
const fakeGitHubSearchBody = `{
  "items": [
    {
      "name": "summarize.json",
      "path": "skills/summarize.json",
      "html_url": "https://github.com/foo/bar/blob/main/skills/summarize.json",
      "repository": {"full_name": "foo/bar", "description": "Summarization helpers"}
    },
    {
      "name": "outside.json",
      "path": "not-skills/outside.json",
      "html_url": "https://github.com/foo/bar/blob/main/not-skills/outside.json",
      "repository": {"full_name": "foo/bar", "description": "Should be skipped"}
    },
    {
      "name": "rewrite.json",
      "path": "skills/rewrite.json",
      "html_url": "https://github.com/baz/qux/blob/main/skills/rewrite.json",
      "repository": {"full_name": "baz/qux", "description": ""}
    }
  ]
}`

func TestGitHubSearch_ParsesAndFilters(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if !strings.Contains(r.URL.RawQuery, "summarize") {
			t.Errorf("query should include user term; got %q", r.URL.RawQuery)
		}
		if !strings.Contains(r.URL.RawQuery, "path") {
			t.Errorf("query should scope to skills path; got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeGitHubSearchBody))
	}))
	defer srv.Close()

	g := &GitHubSource{HTTPClient: srv.Client()}
	// Override the API endpoint by re-routing through the fake server.
	// Real Search() targets api.github.com directly, so we monkey-patch
	// via an HTTP transport that rewrites the host.
	g.HTTPClient = &http.Client{Transport: rewriteHostTo{u: srv.URL, base: srv.Client().Transport}}

	hits, err := g.Search(context.Background(), "summarize", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !called {
		t.Fatal("Search should have hit the fake server")
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits (path-filtered), got %d: %+v", len(hits), hits)
	}
	if hits[0].Ref != "foo/bar:summarize" {
		t.Errorf("hit[0].Ref = %q, want foo/bar:summarize", hits[0].Ref)
	}
	if hits[0].Source != "github" {
		t.Errorf("hit[0].Source = %q, want github", hits[0].Source)
	}
	if hits[1].Ref != "baz/qux:rewrite" {
		t.Errorf("hit[1].Ref = %q, want baz/qux:rewrite", hits[1].Ref)
	}
}

func TestGitHubSearch_RejectsEmptyQuery(t *testing.T) {
	g := NewGitHubSource()
	if _, err := g.Search(context.Background(), "  ", 5); err == nil {
		t.Error("empty query should error")
	}
}

func TestGitHubSearch_RateLimitMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"message":"limit"}`))
	}))
	defer srv.Close()

	g := &GitHubSource{HTTPClient: &http.Client{
		Transport: rewriteHostTo{u: srv.URL, base: srv.Client().Transport},
	}}
	_, err := g.Search(context.Background(), "x", 5)
	if err == nil || !strings.Contains(err.Error(), "rate-limited") {
		t.Errorf("expected rate-limited message; got %v", err)
	}
}

// rewriteHostTo is a tiny http.RoundTripper that swaps a request's
// scheme+host for a target URL — used so we can drive Search() against a
// httptest server without re-implementing the function under test.
type rewriteHostTo struct {
	u    string
	base http.RoundTripper
}

func (r rewriteHostTo) RoundTrip(req *http.Request) (*http.Response, error) {
	parsed, err := http.NewRequest(req.Method, r.u+req.URL.Path+"?"+req.URL.RawQuery, req.Body)
	if err != nil {
		return nil, err
	}
	for k, v := range req.Header {
		parsed.Header[k] = v
	}
	t := r.base
	if t == nil {
		t = http.DefaultTransport
	}
	return t.RoundTrip(parsed)
}
