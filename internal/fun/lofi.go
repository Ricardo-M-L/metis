package fun

// lofi.go — `/fun lofi [preset]` background ambience via mpv. Five
// presets so the user doesn't have to remember YouTube URLs.
//
// All streams are public YouTube live broadcasts; mpv handles HLS
// directly. yt-dlp NOT required for live streams (mpv reads YouTube
// stream URLs natively in 0.36+). If the user's mpv is older we fall
// back to a yt-dlp probe automatically.

import (
	"fmt"
	"os/exec"
	"strings"
)

// presets maps the user-facing preset name to a YouTube live stream
// URL. Keep the list small and curated — too many options is a
// usability tax for "I just want background noise."
var presets = map[string]struct {
	url   string
	title string
}{
	"focus":     {"https://www.youtube.com/watch?v=jfKfPfyJRdk", "Lofi Girl — beats to relax/study"},
	"cafe":      {"https://www.youtube.com/watch?v=Dx5qFachd3A", "Cafe Music BGM — coffee shop jazz"},
	"jazz":      {"https://www.youtube.com/watch?v=Dx5qFachd3A", "Cafe Music BGM — coffee shop jazz"},
	"classical": {"https://www.youtube.com/watch?v=mIYzp5rcTvU", "Classical Music for Studying"},
	"rain":      {"https://www.youtube.com/watch?v=mPZkdNFkNps", "Rain sounds for sleep"},
}

const defaultPreset = "focus"

// lofiCommand handles `/fun lofi [preset|stop]`.
func lofiCommand(arg string) string {
	arg = strings.TrimSpace(strings.ToLower(arg))
	if arg == "stop" {
		return musicCommand("stop") // reuse the single stop path
	}
	preset := defaultPreset
	if arg != "" {
		if _, ok := presets[arg]; !ok {
			return fmt.Sprintf("unknown preset %q. Available: focus, cafe, jazz, classical, rain", arg)
		}
		preset = arg
	}
	entry := presets[preset]

	// Stop any existing player first — only one /fun stream at a
	// time. Mirroring claude-code's player singleton model (one mpv,
	// one Music.app, never both).
	if cur, err := LoadState(); err == nil {
		stopPlayer(cur)
	}

	if _, err := exec.LookPath("mpv"); err != nil {
		return strings.Join([]string{
			"mpv not found on PATH.",
			"Install: brew install mpv  (macOS)  /  apt install mpv  (Linux)",
			"",
			"If you want to keep /fun lofi as a one-liner, mpv is the cheapest dependency — no API keys, no auth.",
		}, "\n")
	}

	pid, err := spawnMpv(entry.url)
	if err != nil {
		return fmt.Sprintf("failed to start mpv: %v", err)
	}
	state := &PlayerState{
		PID:   pid,
		URL:   entry.url,
		Title: entry.title,
	}
	state.StartedAt = nowFunc()
	if err := state.Save(); err != nil {
		// Don't kill the player just because state save failed —
		// it's a quality-of-life feature, not a correctness gate.
		return fmt.Sprintf("playing %s (pid=%d)\n(warning: failed to persist state: %v — `/fun music stop` won't work until next restart)", entry.title, pid, err)
	}
	return fmt.Sprintf("▶ %s\n  pid=%d · `/fun music status` to inspect · `/fun music stop` to kill", entry.title, pid)
}
