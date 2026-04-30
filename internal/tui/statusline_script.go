package tui

// statusline_script.go reproduces claude-code's user-configurable
// status line: if `~/.metis/statusline.sh` (or `.statusline`) is
// executable, metis runs it every `statusLineRefresh` seconds and
// renders its stdout as an extra row at the bottom of the chat
// surface.
//
// Use cases the user can encode in a script:
//   - git branch + dirty/clean state
//   - current task ID (jq into ~/.metis/tasks/<sid>.json)
//   - cost-per-turn dollar estimate
//   - cwd basename (when running multiple metis sessions)
//
// Contract:
//   - exec timeout 1s; output > 1s wins overwritten on next refresh
//   - non-zero exit silently shows nothing
//   - stdout is truncated to terminal width to prevent wrap
//   - env passed: METIS_MODEL, METIS_MODE, METIS_SESSION, METIS_TOKENS_TOTAL,
//     METIS_CWD, METIS_PWD (alias for compat)

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const statusLineRefresh = 5 * time.Second
const statusLineExecTimeout = 1 * time.Second

var (
	statusLineMu      sync.Mutex
	statusLineOutput  string
	statusLineLastRun time.Time
	statusLineRunning bool
)

// statusLineScriptPath returns the resolved executable path of the
// user's status-line script, or "" if no such script exists. Looks
// at $METIS_HOME/statusline.sh (preferred) and $METIS_HOME/statusline.
// We re-resolve every refresh so the user can drop a new script in
// without restarting metis.
func statusLineScriptPath() string {
	home := os.Getenv("METIS_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(h, ".metis")
	}
	for _, name := range []string{"statusline.sh", "statusline"} {
		p := filepath.Join(home, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o100 != 0 {
			return p
		}
	}
	return ""
}

// runStatusLineScript executes the user's status-line script under a
// 1s deadline and stores stdout in statusLineOutput. Designed to be
// kicked off from a goroutine so the UI tick stays cheap. Failures
// (non-zero exit, timeout, bad permissions) silently clear output —
// we don't want a flaky user script to spam errors into the chat.
func runStatusLineScript(model, mode, sessionID string, tokenCount int) {
	statusLineMu.Lock()
	if statusLineRunning {
		statusLineMu.Unlock()
		return // another goroutine already in flight; skip this tick
	}
	if time.Since(statusLineLastRun) < statusLineRefresh {
		statusLineMu.Unlock()
		return
	}
	statusLineRunning = true
	statusLineLastRun = time.Now()
	statusLineMu.Unlock()

	go func() {
		defer func() {
			statusLineMu.Lock()
			statusLineRunning = false
			statusLineMu.Unlock()
		}()
		path := statusLineScriptPath()
		if path == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), statusLineExecTimeout)
		defer cancel()
		cwd, _ := os.Getwd()
		cmd := exec.CommandContext(ctx, path)
		cmd.Env = append(os.Environ(),
			"METIS_MODEL="+model,
			"METIS_MODE="+mode,
			"METIS_SESSION="+sessionID,
			fmt.Sprintf("METIS_TOKENS_TOTAL=%d", tokenCount),
			"METIS_CWD="+cwd,
			"METIS_PWD="+cwd,
		)
		out, err := cmd.Output()
		statusLineMu.Lock()
		defer statusLineMu.Unlock()
		if err != nil {
			statusLineOutput = ""
			return
		}
		// First non-empty line only — the script is meant to produce
		// a single status row, not a stream.
		statusLineOutput = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	}()
}

// statusLineCurrent returns the most recently captured script output.
func statusLineCurrent() string {
	statusLineMu.Lock()
	defer statusLineMu.Unlock()
	return statusLineOutput
}
