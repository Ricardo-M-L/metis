package main

// cmd_resume_picker.go — `metis -r` / `metis --resume` with no UUID
// argument should open an interactive picker over recent sessions
// (claude-code parity, user reference image 2026-05-08). This file
// holds (a) the args pre-scanner that detects the bare-flag gesture
// without confusing Go's `flag` package, and (b) the stdin-driven
// numeric picker.
//
// Picker is intentionally non-TUI: it's a one-shot question before
// the bubbletea program runs. A full bubbletea picker would mean
// starting + tearing down two programs in sequence, and the
// terminal-restore footwork that currently lives in tui.RunTUI
// doesn't compose cleanly with that. A printf + Scan is plenty.

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/session"
)

// liftBareResume walks `args` and removes any `-r` / `--resume` token
// that's NOT followed by a value. Sets *pick=true when it removes one.
// Multi-form input is handled:
//
//	metis -r            → bare → pick=true
//	metis -r -c         → bare → pick=true (next arg is another flag)
//	metis -r abc-123    → has value → left intact for flag.Parse
//	metis --resume xyz  → has value → left intact
//	metis --resume      → bare → pick=true
//	metis --resume=xyz  → has value (=-form) → left intact
func liftBareResume(args []string, pick *bool) []string {
	out := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		a := args[i]
		isResume := a == "-r" || a == "--resume"
		if isResume {
			next := ""
			if i+1 < len(args) {
				next = args[i+1]
			}
			// Bare when there's no next token, OR when the next token
			// looks like a flag (starts with `-`).
			if next == "" || strings.HasPrefix(next, "-") {
				*pick = true
				i++ // skip the lone -r
				continue
			}
		}
		out = append(out, a)
		i++
	}
	return out
}

// runResumePicker prints a numbered list of recent sessions to stderr
// and reads the user's choice from stdin. Returns the chosen session
// id on success; empty string + nil if the user typed `q` / `0` /
// blank to bail out and start a fresh session.
func runResumePicker(store *session.Store) (string, error) {
	const limit = 20
	entries, err := store.List(limit)
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "(no prior sessions — starting fresh)")
		return "", nil
	}
	dim := "\x1b[2;38;5;245m"
	bold := "\x1b[1m"
	reset := "\x1b[0m"
	fmt.Fprintf(os.Stderr, "%sResume which session?%s\n\n", bold, reset)
	for i, e := range entries {
		title := e.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(os.Stderr, "  %s%2d.%s %-12s  %s%s · %s%s\n",
			bold, i+1, reset,
			short12(e.ID),
			dim, e.CreatedAt.Format("2006-01-02 15:04"), title, reset)
	}
	fmt.Fprintf(os.Stderr, "\n%sPick number (or `q` / Enter for fresh):%s ", bold, reset)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		// EOF / closed stdin — start fresh rather than blocking.
		fmt.Fprintln(os.Stderr, "")
		return "", nil
	}
	line = strings.TrimSpace(line)
	if line == "" || line == "q" || line == "Q" || line == "0" {
		fmt.Fprintln(os.Stderr, "(starting fresh)")
		return "", nil
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(entries) {
		fmt.Fprintf(os.Stderr, "(invalid choice %q — starting fresh)\n", line)
		return "", nil
	}
	chosen := entries[n-1].ID
	fmt.Fprintf(os.Stderr, "%s→ resuming %s%s\n", dim, short12(chosen), reset)
	return chosen, nil
}

// short12 truncates a UUID to its first 12 characters for display.
// 12 is enough to disambiguate any two sessions you'd be looking at
// in `metis ps`. Pulled out so callers don't repeat the slice math.
func short12(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
