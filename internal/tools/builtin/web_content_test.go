package builtin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestWebSearchDefaultConfiguredSourcesParallelHTTP(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_TEST_SEARCH_KEY", "test-only")
	started := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-started:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	// Both backend functions must enter before either is allowed to finish.
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	search := func(ctx context.Context, c *http.Client, q, key string, n int) ([]webSearchResult, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		resp.Body.Close()
		return []webSearchResult{{Title: "Local", Link: srv.URL, Snippet: "local result"}}, nil
	}
	old := webSearchBackends
	t.Cleanup(func() { webSearchBackends = old })
	webSearchBackends = []webSearchBackend{{name: "a", envVar: "METIS_TEST_SEARCH_KEY", search: search}, {name: "b", envVar: "METIS_TEST_SEARCH_KEY", search: search}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan *tools.Result, 1)
	go func() { res, _ := (WebSearch{}).Execute(ctx, map[string]any{"query": "local parallel"}); done <- res }()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("configured sources did not start concurrently")
		}
	}
	close(release)
	res := <-done
	if res == nil || res.IsError || !strings.Contains(res.Output, "sources: a, b") {
		t.Fatalf("%+v", res)
	}
}

func TestHTMLMarkdownTableRelativeLinks(t *testing.T) {
	out := htmlMarkdown(`<table><tr><th>Guide</th></tr><tr><td><a href="../api?q=1">API</a><a href="/root">Root</a></td></tr></table>`, "https://example.com/docs/start/")
	for _, want := range []string{"https://example.com/docs/api?q=1", "https://example.com/root", "---"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
}

func TestHTMLMarkdownNestedTableBounded(t *testing.T) {
	const depth = 100
	body := strings.Repeat("<table><tr><td>", depth) + `<a href="../target">deep link</a><script>hidden-script</script>` + strings.Repeat("</td></tr></table>", depth)
	out := htmlMarkdown(body, "https://example.com/docs/page")
	if !strings.Contains(out, "https://example.com/target") || strings.Count(out, "deep link") != 1 || strings.Contains(out, "hidden-script") {
		t.Fatalf("nested content lost or duplicated: %s", out)
	}
	if strings.Count(out, "---") > maxHTMLTableDepth {
		t.Fatalf("nested conversion limit exceeded: %s", out)
	}
	if len(out) > len(body)*2 {
		t.Fatalf("nested output inflated: %d", len(out))
	}
}

func TestHTMLMarkdownStructuralFallback(t *testing.T) {
	out := htmlMarkdown("<html><title>Docs Title</title><body><article>Small</article><main><pre>a```b</pre><table><tr><th>A</th><th>B</th></tr><tr><td>x|y</td><td><a href='https://example.com'>ref</a></td></tr></table><iframe>badframe</iframe></main></body></html>", "https://example.com")
	for _, s := range []string{"# Docs Title", "````", "a```b", "---", "x\\|y", "https://example.com"} {
		if !strings.Contains(out, s) {
			t.Fatalf("lost %q: %s", s, out)
		}
	}
	if strings.Contains(out, "badframe") {
		t.Fatal(out)
	}
}

func TestWebFetchBodyBudgetRecovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	original := `<html><head><style>badstyle</style></head><body><script>badscript</script><div hidden>secret</div><div style="display: none">invisible</div><h1>Guide</h1><article>tiny</article><main><pre><code>fmt.Println("ok")</code></pre><table><tr><th>Key</th><td>Value</td></tr></table><a href="/docs">Docs</a>` + strings.Repeat("正常正文", 2000) + `</main></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, original)
	}))
	defer srv.Close()
	res, err := (WebFetch{}).Execute(context.Background(), map[string]any{"url": srv.URL, "max_output_tokens": 2048})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if webTokenEstimate(res.Output) > 2048 {
		t.Fatalf("over budget: %d", len(res.Output))
	}
	for _, bad := range []string{"badscript", "badstyle", "secret", "invisible"} {
		if strings.Contains(res.Output, bad) {
			t.Fatalf("leak: %s", bad)
		}
	}
	for _, good := range []string{"# Guide", "```", "Key", "Value", srv.URL + "/docs"} {
		if !strings.Contains(res.Output, good) {
			t.Fatalf("lost %s: %s", good, res.Output)
		}
	}
	files, _ := filepath.Glob(filepath.Join(home, "tool-results", "*.spill.txt"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	raw, _ := os.ReadFile(files[0])
	if string(raw) != original {
		t.Fatal("original not recoverable")
	}
	st, _ := os.Stat(files[0])
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}

func TestWebFetchDownloadBoundaryAndRaw(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<p>abc</p>")
	}))
	defer srv.Close()
	for _, tt := range []struct {
		limit     int
		truncated bool
	}{{10, false}, {9, true}} {
		res, err := (WebFetch{}).Execute(context.Background(), map[string]any{"url": srv.URL, "max_bytes": tt.limit, "mode": "raw"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(res.Output, "download truncated") != tt.truncated {
			t.Fatal(res.Output)
		}
		if !strings.Contains(res.Output, "<p>abc") {
			t.Fatal(res.Output)
		}
	}
}

func TestWebSearchMultiMergeBudget(t *testing.T) {
	old := webSearchBackends
	t.Cleanup(func() { webSearchBackends = old })
	webSearchBackends = []webSearchBackend{
		{name: "a", search: func(context.Context, *http.Client, string, string, int) ([]webSearchResult, error) {
			return []webSearchResult{{Title: "One", Link: "https://EXAMPLE.com/doc?utm_source=a", Snippet: strings.Repeat("long", 1000)}}, nil
		}},
		{name: "b", search: func(context.Context, *http.Client, string, string, int) ([]webSearchResult, error) {
			return []webSearchResult{{Title: "Same", Link: "https://example.com/doc#part", Snippet: "other"}}, nil
		}},
	}
	res, err := (WebSearch{}).Execute(context.Background(), map[string]any{"query": "test", "sources": []string{"a", "b"}, "max_output_tokens": 1024})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if !strings.Contains(res.Output, "1 results") || !strings.Contains(res.Output, "sources: a, b") || webTokenEstimate(res.Output) > 1024 {
		t.Fatal(res.Output)
	}
}

func TestWebSearchReuseSuccessFailureFresh(t *testing.T) {
	var calls atomic.Int32
	run := func(context.Context, map[string]any) (*tools.Result, error) {
		calls.Add(1)
		return &tools.Result{Output: "result", Meta: map[string]any{"search_reusable": true}}, nil
	}
	ctx := tasks.WithSessionID(context.Background(), "reuse-"+t.Name())
	for i := 0; i < 2; i++ {
		_, _ = executeBudgetedSearch(ctx, map[string]any{"query": "stable"}, run)
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	_, _ = executeBudgetedSearch(ctx, map[string]any{"query": "stable", "fresh": true}, run)
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
	fail := func(context.Context, map[string]any) (*tools.Result, error) {
		calls.Add(1)
		return &tools.Result{Output: "failure", IsError: true}, nil
	}
	for i := 0; i < 2; i++ {
		_, _ = executeBudgetedSearch(ctx, map[string]any{"query": "failed"}, fail)
	}
	if calls.Load() != 4 {
		t.Fatal(calls.Load())
	}
	old := webSearchBackends
	t.Cleanup(func() { webSearchBackends = old })
	webSearchBackends = []webSearchBackend{{name: "test", envVar: "METIS_TEST_SEARCH_IDENTITY"}}
	t.Setenv("METIS_TEST_SEARCH_IDENTITY", "first")
	_, _ = executeBudgetedSearch(ctx, map[string]any{"query": "identity"}, run)
	t.Setenv("METIS_TEST_SEARCH_IDENTITY", "second")
	_, _ = executeBudgetedSearch(ctx, map[string]any{"query": "identity"}, run)
	if calls.Load() != 6 {
		t.Fatal("key change reused stale result", calls.Load())
	}
	empty := func(context.Context, map[string]any) (*tools.Result, error) {
		calls.Add(1)
		return &tools.Result{Output: "empty", Meta: map[string]any{"search_reusable": false}}, nil
	}
	for i := 0; i < 2; i++ {
		_, _ = executeBudgetedSearch(ctx, map[string]any{"query": "empty"}, empty)
	}
	if calls.Load() != 8 {
		t.Fatal("empty cached", calls.Load())
	}
}
