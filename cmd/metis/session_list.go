package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/session"
)

type sessionListOptions struct {
	limit      int
	jsonOutput bool
	workDir    string
}

func parseSessionListOptions(args []string) (sessionListOptions, error) {
	opts := sessionListOptions{limit: 20}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			opts.jsonOutput = true
		case "--limit", "-n":
			if i+1 >= len(args) {
				return sessionListOptions{}, errors.New("sessions list: --limit requires a positive integer")
			}
			n, ok := atoiSafe(args[i+1])
			if !ok || n < 1 {
				return sessionListOptions{}, fmt.Errorf("sessions list: invalid limit %q", args[i+1])
			}
			opts.limit = n
			i++
		case "--work-dir":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return sessionListOptions{}, errors.New("sessions list: --work-dir requires a path")
			}
			workDir, err := filepath.Abs(args[i+1])
			if err != nil {
				return sessionListOptions{}, fmt.Errorf("sessions list: resolve work dir: %w", err)
			}
			opts.workDir = workDir
			i++
		default:
			return sessionListOptions{}, fmt.Errorf("sessions list: unknown option %q", args[i])
		}
	}
	return opts, nil
}

// listSessionEntries applies the workspace predicate before the display
// limit. This matters for desktop clients: globally newer sessions from
// another repository must not evict an older session from the active one.
// Legacy headers without WorkDir remain visible for backwards compatibility.
func listSessionEntries(store *session.Store, opts sessionListOptions) ([]session.ListEntry, error) {
	return store.ListResumable(session.ResumeListOptions{Limit: opts.limit, WorkDir: opts.workDir})
}

func sameSessionWorkDir(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil {
		return os.SameFile(leftInfo, rightInfo)
	}
	leftAbs, leftAbsErr := filepath.Abs(left)
	rightAbs, rightAbsErr := filepath.Abs(right)
	if leftAbsErr == nil {
		left = leftAbs
	}
	if rightAbsErr == nil {
		right = rightAbs
	}
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if goruntime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
