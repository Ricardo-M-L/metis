package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLightThemeKeepsDesktopChromeReadableAndSyncsNativeWindow(t *testing.T) {
	s, _ := testServer(t)
	get := func(path string) string {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	style := get("/style.css")
	for _, want := range []string{
		`[data-theme="light"]`,
		`--new-session-bg`,
		`--new-session-hover`,
		`background: var(--new-session-bg)`,
		`.new-session-btn:hover { background: var(--new-session-hover)`,
	} {
		if !strings.Contains(style, want) {
			t.Fatalf("style.css missing light-theme chrome guard %q", want)
		}
	}

	chat := get("/chat.js")
	for _, want := range []string{
		`requestNative('set-theme'`,
		`theme: resolved`,
		`document.documentElement.dataset.theme = resolved`,
	} {
		if !strings.Contains(chat, want) {
			t.Fatalf("chat.js missing native theme synchronization %q", want)
		}
	}
}
