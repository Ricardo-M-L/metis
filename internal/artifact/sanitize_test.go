package artifact

import (
	"strings"
	"testing"
)

func TestSanitizeHTMLKeepsStaticMarkupAndDropsCapabilities(t *testing.T) {
	raw := `<!doctype html><html><head>
<style>@import "https://evil.invalid/x.css"; .safe { color: red; background: url(https://evil.invalid/pixel) } .also { display: grid }</style>
</head><body class="page" onload="evil()">
<custom-widget data-secret="x"><strong>visible</strong></custom-widget>
<form action="https://evil.invalid"><input autofocus><button>send</button></form>
<svg><script>alert(1)</script></svg><math><mi>x</mi></math>
<a id="top" href="#top" target="_top">local</a>
<a href="javascript:alert(1)">js</a><a href="https://evil.invalid">remote</a>
<img alt="ok" src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB">
<img src="data:image/svg+xml;base64,PHN2Zz48c2NyaXB0Lz48L3N2Zz4=">
<p style="color: blue; background-image: url(https://evil.invalid/x)">styled</p>
</body></html>`

	clean, err := SanitizeHTML(raw)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(clean)
	for _, forbidden := range []string{
		"evil.invalid", "javascript:", "<script", "<iframe", "<form", "<input", "<button",
		"<svg", "<math", "<custom-widget", "onload=", "target=", "data-secret=", "image/svg+xml", "url(", "@import",
	} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("sanitized HTML retained %q:\n%s", forbidden, clean)
		}
	}
	for _, wanted := range []string{"<strong>visible</strong>", `href="#top"`, "display: grid", "color: blue", "data:image/png;base64"} {
		if !strings.Contains(lower, strings.ToLower(wanted)) {
			t.Errorf("sanitized HTML lost %q:\n%s", wanted, clean)
		}
	}
}

func TestSanitizeHTMLRejectsExpansionBeyondLimit(t *testing.T) {
	// Entity decoding and normalized document wrappers can make rendered output
	// larger than the raw input. Both sides of the sanitizer boundary are capped.
	raw := strings.Repeat("&amp;", MaxHTMLBytes/5)
	if _, err := SanitizeHTML(raw); err == nil {
		t.Fatal("expected rendered HTML over the 2 MiB limit to be rejected")
	}
}
