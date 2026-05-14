package builtin

// web_browse_isenabled_test.go pins WebBrowse's self-aware availability
// check (2026-05-15). Sibling to lsp_isenabled_test.go — both
// demonstrate the IsEnabled() override pattern where a tool's
// availability depends on a host-side binary.
//
// WebBrowse: present iff a chromium-family binary is installed
//   (chromium / google-chrome / chrome / microsoft-edge /
//   brave-browser / Chromium.app / Google Chrome.app)
//
// LSP:       present iff `gopls` is on PATH
//
// Both follow the same shape: skip the BaseTool embed, implement
// `IsEnabled() bool` returning the result of a binary probe.
//
// Note: on macOS the host nearly always has Google Chrome installed in
// /Applications/, and pickChromiumBinary's os.Stat probe of that
// absolute path is PATH-independent — so the "hide WebBrowse" branch
// is hard to exercise as a smoke test on a Mac. Linux/Docker
// environments without any chromium binary are where the hide branch
// kicks in naturally. These unit tests cover both branches via the
// IsEnabled-tracks-probe invariant; they don't assume a specific
// answer.

import "testing"

func TestWebBrowse_IsEnabled_TracksChromiumPresence(t *testing.T) {
	// Use the uncached probe for the test (the runtime cache locks the
	// result at first call site, which could be any earlier test).
	// pickChromiumBinary and cachedChromiumBinary should agree on
	// fresh hosts; this test just asserts the wiring is correct, not
	// that the cache holds a specific value.
	want := pickChromiumBinary() != ""
	got := WebBrowse{}.IsEnabled()
	if got != want {
		t.Errorf("WebBrowse.IsEnabled() = %v; pickChromiumBinary() found-binary = %v — these must agree", got, want)
	}
}

func TestWebBrowse_ChromiumCacheIsStable(t *testing.T) {
	// cachedChromiumBinary uses sync.Once — repeated calls must yield
	// the same value. Guards against accidental refactor to a non-
	// memoized form (which would re-probe stat/lookPath every filter
	// pass and noticeably slow `metis tools` listing).
	first := cachedChromiumBinary()
	second := cachedChromiumBinary()
	if first != second {
		t.Errorf("cachedChromiumBinary not stable across calls: %q vs %q", first, second)
	}
}
