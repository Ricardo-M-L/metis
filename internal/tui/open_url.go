package tui

// open_url.go — best-effort "open this URL in the user's browser" for
// the OSC 8 click-to-open path in tui_update.go.
//
// Uses the platform's standard URL opener: `open` on macOS,
// `xdg-open` on Linux, `cmd /c start` on Windows. Run detached so the
// browser launching doesn't block the TUI event loop. We do NOT inherit
// stdin/stdout — `open` would fight with our alt-screen rendering, and
// most browser openers fork the actual launch anyway.

import (
	"fmt"
	goruntime "runtime"

	osexec "os/exec"
)

// openURL launches url in the user's default handler. Returns an error
// when the spawn itself failed (binary not found, exec denied) — most
// other failures (browser refused to open, URL malformed) happen async
// after the spawned process detaches, so this is best-effort.
//
// Security: url is taken straight from OSC 8 escapes embedded in the
// rendered chat content. Those escapes can come from tool output or
// model responses, so a hostile assistant could craft a URL that does
// something nasty when "opened." Mitigations:
//
//  1. We never pass the URL through a shell. Each platform launcher
//     gets the URL as a single argument, so quoting tricks (`;rm -rf`,
//     backticks, $()) don't work.
//  2. Reject anything that doesn't look like a URL or local path.
//     `file://` paths and `http(s)://` are allowed; everything else is
//     blocked. (We deliberately allow `file://` because tool results
//     often link local files.)
//  3. The user has to single-left-click on the link in the chat to
//     trigger this — they have a clear UI affordance, and a stray
//     open of a malicious URL only gets one shot.
func openURL(url string) error {
	if !isAllowedURL(url) {
		return fmt.Errorf("scheme not permitted")
	}
	var cmd *osexec.Cmd
	switch goruntime.GOOS {
	case "darwin":
		cmd = osexec.Command("open", url)
	case "linux":
		cmd = osexec.Command("xdg-open", url)
	case "windows":
		// `cmd /c start` is the canonical Windows URL opener; the
		// extra empty quoted arg avoids `start` interpreting the URL
		// as the window title.
		cmd = osexec.Command("cmd", "/c", "start", "", url)
	default:
		return fmt.Errorf("unsupported platform: %s", goruntime.GOOS)
	}
	return cmd.Start()
}

// isAllowedURL gates which schemes we'll hand to the OS opener. Keeps
// the trust model tight: a malicious tool output trying to fire
// `javascript:fetch(...)` or `vbscript:` simply doesn't open. We accept
// `http://` and `https://` (the 99% case) and `file://` (tool results
// that link local files — explicitly opt-in here because users expect
// to be able to click filenames in Read / Bash output).
func isAllowedURL(url string) bool {
	prefixes := []string{"http://", "https://", "file://"}
	for _, p := range prefixes {
		if len(url) >= len(p) && url[:len(p)] == p {
			return true
		}
	}
	return false
}
