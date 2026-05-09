package main

// cmd_dirs.go — `metis dirs` introspection command.
//
// Prints the canonical paths metis uses for config, data, sessions
// (the per-session jsonl transcript dir), and logs. Useful for shell
// composition (`tail -f $(metis dirs logs)/debug.log`,
// `cd $(metis dirs sessions)`) and debugging "where did metis put
// my X?" without grepping the source.
//
// Behavior mirrors crush's `crush dirs` (internal/cmd/dirs.go):
//
//   metis dirs              Print all four dirs as a table (TTY) or
//                           one-per-line key=value (pipe).
//   metis dirs <key>        Print only the path for <key>, no
//                           formatting — designed for shell capture.
//
// Recognised keys: config, data, sessions, logs. Anything else is a
// usage error.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// cmdDirs is the entry point — dispatches to the all-dirs table or
// the single-key path output.
func cmdDirs(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(os.Stderr, dirsUsage)
		return nil
	}
	dirs := resolveDirs()
	if len(args) == 0 {
		printDirsAll(os.Stdout, dirs)
		return nil
	}
	key := strings.ToLower(args[0])
	path, ok := dirs[key]
	if !ok {
		return fmt.Errorf("metis dirs: unknown key %q (try one of: config, data, sessions, logs)", args[0])
	}
	fmt.Fprintln(os.Stdout, path)
	return nil
}

// resolveDirs centralises the four canonical paths — kept here rather
// than building from config.Home() at every print site so a future
// change to layout (XDG migration redux, sandboxed install) only
// touches one function.
func resolveDirs() map[string]string {
	cfg, _, err := config.Load()
	home := config.Home()

	sessionsDir := filepath.Join(home, "sessions")
	if err == nil && cfg != nil && cfg.Session.Dir != "" {
		sessionsDir = cfg.Session.Dir
	}

	return map[string]string{
		"config":   filepath.Join(home, "config.toml"),
		"data":     home,
		"sessions": sessionsDir,
		"logs":     filepath.Join(home, "logs"),
	}
}

// printDirsAll prints the four paths in a stable order. TTY output
// uses an aligned table; non-TTY output is `key\tpath` so awk/cut
// pipelines stay simple.
func printDirsAll(w io.Writer, dirs map[string]string) {
	keys := []string{"config", "data", "sessions", "logs"}
	if isTerminal(w) {
		// Compute column width so every value lines up.
		const labelWidth = 9 // longest is "sessions:" with trailing colon
		for _, k := range keys {
			fmt.Fprintf(w, "  %-*s %s\n", labelWidth, k+":", dirs[k])
		}
		return
	}
	for _, k := range keys {
		fmt.Fprintf(w, "%s\t%s\n", k, dirs[k])
	}
}

// isTerminal returns true when w is an *os.File pointing at a TTY.
// Anything else (pipe, file, bytes.Buffer in tests) gets the plain
// tab-separated output. Crush has the same split.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

const dirsUsage = `usage: metis dirs [config|data|sessions|logs]

Print canonical metis directories.

  metis dirs              List all four (table on TTY, key=path on pipe)
  metis dirs config       Path to config.toml
  metis dirs data         Top-level metis home (~/.metis)
  metis dirs sessions     Where session transcripts live
  metis dirs logs         Log directory

Examples:
  cd $(metis dirs sessions)
  tail -f $(metis dirs logs)/debug.log
  ls $(metis dirs data)`

// Keep an explicit error for the unknown-key path so tests can match it.
var errUnknownDirsKey = errors.New("unknown dirs key")
