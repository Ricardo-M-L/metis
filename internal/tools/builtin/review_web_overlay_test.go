package builtin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// These regressions originated in the context review's independent overlay.
func TestReviewWebFetchRedirectRelativeLink(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/docs/page", http.StatusFound)
			return
		}
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><main><p>Article content</p><a href="next.html">Next</a><table><tr><td><a href="../api">API</a></td></tr></table></main></body></html>`)
	}))
	defer srv.Close()
	result, err := (WebFetch{http: srv.Client()}).Execute(context.Background(), map[string]any{"url": srv.URL + "/start"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{srv.URL + "/docs/next.html", srv.URL + "/api"} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("relative link did not use final response URL %q: %s", want, result.Output)
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("redirected retry attempts = %d, want 2", attempts.Load())
	}
}

func TestReviewHiddenTableContentExcluded(t *testing.T) {
	for _, tag := range []string{"table", "thead", "tbody", "tfoot", "tr", "td", "th"} {
		for _, attr := range []string{"hidden", `aria-hidden="true"`, `style="display: none"`, `style="visibility: hidden"`} {
			t.Run(tag+"/"+attr, func(t *testing.T) {
				cell := `<td>HIDDEN_CONTENT<table><tr><td>HIDDEN_NESTED</td></tr></table></td>`
				var hidden string
				switch tag {
				case "td", "th":
					hidden = `<table><tr><` + tag + ` ` + attr + `>HIDDEN_CONTENT<table><tr><td>HIDDEN_NESTED</td></tr></table></` + tag + `></tr></table>`
				case "tr":
					hidden = `<table><tr ` + attr + `>` + cell + `</tr></table>`
				case "table":
					hidden = `<table ` + attr + `><tr>` + cell + `</tr></table>`
				default:
					hidden = `<table><` + tag + ` ` + attr + `><tr>` + cell + `</tr></` + tag + `></table>`
				}
				out := htmlMarkdown(hidden+`<table><tr><td>visible<a href="next">Next</a></td></tr></table>`, "https://fixture.example/docs/")
				if strings.Contains(out, "HIDDEN_") || !strings.Contains(out, "visible") || !strings.Contains(out, "https://fixture.example/docs/next") {
					t.Fatalf("table visibility/link regression: %s", out)
				}
			})
		}
	}
}

func TestHTMLMarkdownTableVisibleCodeAndNoncontent(t *testing.T) {
	out := htmlMarkdown(`<table><tr hidden><td>HIDDEN_ROW</td></tr><tr><td hidden>HIDDEN_CELL</td><td>visible<pre><code>a  b
c&lt;d</code><span hidden>HIDDEN_CODE</span></pre><script>HIDDEN_SCRIPT</script><template>HIDDEN_TEMPLATE</template><iframe>HIDDEN_FRAME</iframe><span style="display:
 none">HIDDEN_STYLE</span><table><tr aria-hidden="true"><td>HIDDEN_DEEP</td></tr><tr><td><a href="next">nested</a></td></tr></table></td></tr></table>`, "https://fixture.example/docs/")
	for _, want := range []string{"visible", "```", "a b c<d", "https://fixture.example/docs/next"} {
		if !strings.Contains(out, want) {
			t.Fatalf("lost visible code or link %q: %s", want, out)
		}
	}
	if strings.Contains(out, "HIDDEN_") {
		t.Fatalf("hidden nested/code content leaked: %s", out)
	}
}

func TestWebFetchRedirectStillBlocksPrivateAddress(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()
	result, err := (WebFetch{}).Execute(context.Background(), map[string]any{"url": srv.URL})
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, "WebFetch blocked") {
		t.Fatalf("redirect SSRF protection lost: result=%+v err=%v", result, err)
	}
}
