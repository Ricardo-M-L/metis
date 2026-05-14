// Package fun — opt-in user-delight commands (music / pet / break /
// haiku). Lives outside internal/slash so the slash layer can stay
// thin: slash/fun.go is a 30-line dispatcher that calls into this
// package, and this package owns all the side-effect logic (spawn
// mpv, track PIDs, write state files).
//
// Design principles (see ~/.claude/plans/modular-painting-simon.md
// for the full /fun-system rationale):
//
//   - Subprocess (mpv, osascript) is detached: spawned via
//     os/exec.Start + Process.Release so it survives metis exit.
//     The user's music doesn't die when they close the agent — that
//     would be a bad first impression.
//   - State on disk: ~/.metis/fun/music_state.json tracks the live
//     player so subsequent `/fun music status` / `stop` calls can
//     find it across metis invocations.
//   - All output goes to /dev/null; mpv stderr ends up in
//     ~/.metis/fun/mpv.log for debugging without polluting the TUI.
//   - Zero hard dep on external binaries — Lofi.Start probes mpv and
//     returns a clean "brew install mpv" hint when absent.
package fun

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FunDir returns the on-disk state directory (created on demand).
// Mirrors session.Dir / memdir patterns: ~/.metis/fun/.
func FunDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(home, ".metis", "fun")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}

// Dispatch is the single entry point called by slash/fun.go. Splits
// `args` on the first whitespace, routes to the right subcommand.
// Returns a user-visible message; errors are folded into the message
// (Handler signature doesn't carry a separate error path).
func Dispatch(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return helpText()
	}
	sub, rest, _ := strings.Cut(args, " ")
	rest = strings.TrimSpace(rest)
	switch sub {
	case "help", "?":
		return helpText()
	case "lofi":
		return lofiCommand(rest)
	case "music":
		return musicCommand(rest)
	default:
		return fmt.Sprintf("unknown subcommand %q — try `/fun help`", sub)
	}
}

func helpText() string {
	return strings.TrimSpace(`
/fun — opt-in delight commands.

  /fun lofi               start lofi-girl background stream (mpv)
  /fun lofi stop          stop the stream
  /fun music status       show what's playing right now
  /fun music stop         stop any /fun-managed player

Requires mpv on PATH (brew install mpv). The player runs detached
from metis — closing this session does NOT stop the music.
`)
}

// ErrNotRunning is returned by state.Load when there's no live
// player tracked. Distinguishes "nothing playing" (graceful) from
// "I/O failure reading state file" (surface to user).
var ErrNotRunning = errors.New("no /fun player is running")
