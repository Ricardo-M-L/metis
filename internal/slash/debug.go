package slash

// debug.go — /debug slash command. claude-code parity implementation:
// instead of returning a string for the REPL to render (metis's old
// behavior), this command synthesises a full diagnostic prompt that
// includes the user's issue description, the tail of ~/.metis/debug.log,
// and concrete instructions for the agent. Returning SignalCustomPrompt
// routes that prompt through the agent loop, so the model itself
// analyses the log and proposes fixes — exactly what claude-code's
// /debug skill does (restored-src/src/skills/bundled/debug.ts).
//
// Why this file lives under internal/slash and not internal/tui:
// the TUI's REPLCommand Handler signature only returns a string for
// display; it can't inject a synthesized user message into the agent
// loop. The slash package's SignalCustomPrompt path is the existing
// mechanism for that (used by user-authored ~/.metis/commands/*.md).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
)

const (
	// debugTailBytes caps how much of the debug log we read. Matches
	// claude-code's TAIL_READ_BYTES — large enough to capture the last
	// few errors plus surrounding context, small enough to not blow up
	// the prompt on a multi-MB log.
	debugTailBytes = 64 * 1024
	// debugTailLines caps the number of trailing lines included in the
	// prompt. 20 lines is enough to fingerprint a typical crash without
	// drowning the model in noise (claude-code's DEFAULT_DEBUG_LINES_READ).
	debugTailLines = 20
)

// RegisterDebugCommand wires /debug into the slash registry. Split out
// of RegisterAll so the handler can read config.Home() at command time
// (not registry-buildime) — lets tests override HOME without
// rebuilding the registry.
func RegisterDebugCommand(r *Registry) {
	r.Register(Cmd{
		Name:        "debug",
		Description: "ask the agent to diagnose an issue using ~/.metis/debug.log",
		Handler:     debugHandler,
	})
}

func debugHandler(args string) (string, Signal) {
	logPath := filepath.Join(config.Home(), "debug.log")

	// Build the log-tail section. Mirrors claude-code's "if ENOENT,
	// say logging was just enabled" pattern: metis can't toggle
	// logging mid-process (debug_log.go is opened in main), so the
	// hint is "restart with --debug" instead of "we just enabled it".
	var logSection strings.Builder
	info, err := os.Stat(logPath)
	if err != nil || info.Size() == 0 {
		logSection.WriteString(fmt.Sprintf(
			"The debug log at `%s` is empty or does not exist. Debug logging is NOT capturing for this session — metis only writes the log when started with `--debug` or `METIS_DEBUG=1`. Tell the user to restart with `metis --debug` (or set the env) and reproduce the issue before re-running /debug.",
			logPath))
	} else {
		logSection.WriteString(fmt.Sprintf("The debug log is at `%s` (%d bytes, modified %s).\n\n",
			logPath, info.Size(), info.ModTime().Format("2006-01-02 15:04:05")))
		tail := readLogTail(logPath, info.Size(), debugTailBytes, debugTailLines)
		if tail == "" {
			logSection.WriteString("(could not read log tail)\n")
		} else {
			logSection.WriteString(fmt.Sprintf("### Last %d lines of debug.log\n\n```\n%s\n```\n",
				len(strings.Split(strings.TrimRight(tail, "\n"), "\n")), tail))
		}
	}

	issue := strings.TrimSpace(args)
	if issue == "" {
		issue = "The user did not describe a specific issue. Read the debug log and summarize any errors, warnings, or notable patterns you find."
	}

	prompt := fmt.Sprintf(`# Debug Task

Help the user diagnose an issue in this metis session.

## Session Debug Log

%s

## Issue Description

%s

## Settings

metis settings live in:
- user config: ~/.metis/config.toml
- project config: ./.metis/config.toml (when present)
- protected credentials: ~/.metis/.credentials/
- trusted directories: ~/.metis/trusted-dirs.json

## Instructions

1. Review the user's issue description carefully.
2. Scan the debug log tail above for [ERROR], [WARN], "panic", "fail", "denied", or stack traces. If the tail is short, also Grep the full log file for those patterns.
3. If the log shows the logging is not capturing, tell the user to restart with `+"`metis --debug`"+` and reproduce the issue, then re-run /debug.
4. Explain what you found in plain language — quote the exact log lines that support your diagnosis.
5. Suggest concrete fixes or next steps (config change, restart, file to inspect, GitHub issue search).
6. If the issue is ambiguous, ask the user a targeted follow-up question rather than guessing.
`, logSection.String(), issue)

	return prompt, SignalCustomPrompt
}

// readLogTail returns up to `maxLines` lines from the end of the file
// at `path`, reading at most `maxBytes` from the tail. Empty string on
// any error (caller treats that as "log unavailable").
func readLogTail(path string, size int64, maxBytes, maxLines int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	start := size - int64(maxBytes)
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, 0); err != nil {
		return ""
	}
	buf := make([]byte, size-start)
	n, _ := f.Read(buf)
	buf = buf[:n]
	lines := strings.Split(string(buf), "\n")
	// Drop empty trailing lines and trim to maxLines.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}
