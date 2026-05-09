//go:build darwin

package tui

// notify_apple.go — probe Apple Terminal's audible-bell setting so
// SendNotification can decide whether BEL is safe to emit (silent
// visual bell) or would spam the user with sound.
//
// Mirrors claude-code's services/notifier.ts::isAppleTerminalBellDisabled
// (lines 110-156) but skips the plist library — defaults' text output
// has a stable enough format that a one-shot regex over the named
// profile's block does the job in ~30 lines, no extra dep.
//
// Why we care: Apple Terminal is the macOS default and ships with
// audible bell ON. Emitting BEL with the default profile makes a
// "ding" — annoying as a notification. But power users who turn the
// audible bell off in Preferences → Profiles → Advanced get a silent
// visual bell instead (window-icon flash, dock badge). For them BEL
// is the only viable notification channel.
//
// Probe failure is conservative: any error → "audible bell is on" →
// notification suppressed. We'd rather miss a banner than ring an
// unexpected bell.

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// isAppleTerminalAudibleBellDisabled returns true ONLY when the
// active Terminal.app window's profile has the `Bell` field
// explicitly set to 0 (audible bell off). Any error path or
// missing field returns false.
func isAppleTerminalAudibleBellDisabled() bool {
	profile, ok := currentTerminalProfile()
	if !ok || profile == "" {
		return false
	}
	return profileBellExplicitlyOff(profile)
}

// currentTerminalProfile asks Terminal.app for the front window's
// active profile name via osascript. 3-second timeout in case
// Terminal.app is slow to respond (e.g. first launch of the day
// while it loads the scripting bridge).
func currentTerminalProfile() (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "osascript", "-e",
		`tell application "Terminal" to name of current settings of front window`)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// profileBellExplicitlyOff scans `defaults read com.apple.Terminal
// "Window Settings"` output for the named profile's block and looks
// for `Bell = 0;`. The output format is stable plist-text — we don't
// need a real parser since we only care about one boolean.
//
// Output shape (truncated):
//
//	{
//	    Basic =     {
//	        ...
//	        Bell = 0;
//	        ...
//	    };
//	    "Pro" =     {
//	        ...
//	    };
//	}
//
// Profile names with spaces / special chars get quoted; we match
// either form.
func profileBellExplicitlyOff(profile string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "defaults", "read",
		"com.apple.Terminal", "Window Settings")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return scanBellInBlock(string(out), profile) == "0"
}

// scanBellInBlock walks the defaults output until it finds the named
// profile's `<name> = {` heading, then looks for `Bell = N;` inside
// the matching {…} block. Returns "0", "1", or "" (not present /
// not found).
//
// Brace-counting handles nested dicts inside the profile (e.g. font
// metadata blocks) so we don't fall out of the block prematurely.
func scanBellInBlock(text, profile string) string {
	heading := regexp.MustCompile(`^\s*"?` + regexp.QuoteMeta(profile) + `"?\s*=\s*\{`)
	bellLine := regexp.MustCompile(`^\s*Bell\s*=\s*([01])\s*;`)
	inBlock := false
	depth := 0
	for _, line := range strings.Split(text, "\n") {
		if !inBlock {
			if heading.MatchString(line) {
				inBlock = true
				depth = 1
			}
			continue
		}
		if m := bellLine.FindStringSubmatch(line); m != nil {
			return m[1]
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth <= 0 {
			return ""
		}
	}
	return ""
}
