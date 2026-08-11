package fun

// music.go — /fun music subcommands. Phase 1 carries `status` and
// `stop`; Phase 1b will add `play <query>` (yt-dlp search + spawn),
// `pause` / `resume` / `skip` (via mpv IPC socket).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// nowFunc is overridable in tests so FormatUptime returns predictable
// values when the test sets a fixed clock.
var nowFunc = time.Now

func musicCommand(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		args = "status"
	}
	sub, rest, _ := strings.Cut(args, " ")
	rest = strings.TrimSpace(rest)
	switch sub {
	case "status", "now":
		return musicStatus()
	case "stop":
		return musicStop()
	case "play", "pause", "resume", "skip", "vol", "queue", "history":
		// Phase 1b stubs — register the subcommands so the help
		// text is honest about what we expose, but they fail loudly
		// instead of pretending to work.
		_ = rest
		return fmt.Sprintf("`/fun music %s` is Phase 1b — coming next. For now: `/fun lofi [preset]` starts a stream, `/fun music stop` ends it.", sub)
	default:
		return fmt.Sprintf("unknown music subcommand %q. Try status / stop / play / pause / skip.", sub)
	}
}

func musicStatus() string {
	s, err := LoadState()
	if err == ErrNotRunning {
		return "no /fun-managed player is running"
	}
	if err != nil {
		return fmt.Sprintf("status error: %v", err)
	}
	return fmt.Sprintf("▶ %s\n  pid=%d · running for %s\n  url=%s", s.Title, s.PID, s.FormatUptime(), s.URL)
}

func musicStop() string {
	s, err := LoadState()
	if err == ErrNotRunning {
		return "nothing to stop — no /fun-managed player is running"
	}
	if err != nil {
		return fmt.Sprintf("stop error: %v", err)
	}
	if err := stopPlayer(s); err != nil {
		return fmt.Sprintf("kill pid %d failed: %v (state cleared anyway)", s.PID, err)
	}
	_ = ClearState()
	return fmt.Sprintf("⏹ stopped %q (pid=%d)", s.Title, s.PID)
}

// stopPlayer kills the recorded player process. Tries SIGTERM first
// (mpv handles it gracefully and writes shutdown logs); falls back
// to SIGKILL if it's still alive after 1s.
func stopPlayer(s *PlayerState) error {
	if s == nil || s.PID <= 0 {
		return nil
	}
	proc, err := os.FindProcess(s.PID)
	if err != nil {
		return err
	}
	_ = terminatePlayer(proc)
	// brief settle window
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(s.PID) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Stubborn — escalate.
	return killPlayer(proc)
}

// spawnMpv launches mpv detached so it survives metis exit. Returns
// the PID. stdout/stderr redirected to ~/.metis/fun/mpv.log so we
// can diagnose stream failures without polluting the TUI.
//
// Detach mechanics:
//   - Setpgid puts mpv in its own process group → metis Ctrl+C
//     doesn't propagate to it
//   - Process.Release lets the Go runtime forget the child so it
//     won't appear as a zombie when metis exits
func spawnMpv(url string) (int, error) {
	d, err := FunDir()
	if err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(filepath.Join(d, "mpv.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer logf.Close()

	cmd := exec.Command("mpv",
		"--no-terminal",  // no curses TUI
		"--no-video",     // audio only — saves CPU and lets the YouTube live stream do background play
		"--really-quiet", // skip startup banner
		url,
	)
	cmd.Stdout = logf
	cmd.Stderr = logf
	configureDetachedPlayer(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	// Critical: Release so the Go runtime doesn't track the
	// process. Without this, metis would reap it on exit and the
	// music would stop.
	if err := cmd.Process.Release(); err != nil {
		return pid, err
	}
	return pid, nil
}
