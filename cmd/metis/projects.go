package main

// cmd_projects.go — `metis projects` introspection.
//
// Lists every directory metis has been used in, sorted by last
// access. Backed by internal/projects (registry at
// ~/.metis/projects.json). Designed for shell composition:
//
//   metis projects             Aligned table on TTY, key=value pipes
//   metis projects --json      Machine-readable list
//   metis projects --remove <path>
//                              Drop a stale entry
//
// Auto-registration happens elsewhere — every metis chat / run starts
// up via cmd/metis/main.go which calls projects.Register(cwd, home).
// This command is read-only by default; --remove is the only writer.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/projects"
)

func cmdProjects(args []string) error {
	fs := flag.NewFlagSet("projects", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	remove := fs.String("remove", "", "remove a path from the registry")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *remove != "" {
		abs, err := filepath.Abs(*remove)
		if err != nil {
			return fmt.Errorf("resolve %q: %w", *remove, err)
		}
		removed, err := projects.Remove(abs)
		if err != nil {
			return err
		}
		if removed {
			fmt.Fprintf(os.Stdout, "removed %s\n", abs)
		} else {
			fmt.Fprintf(os.Stdout, "no entry for %s\n", abs)
		}
		return nil
	}

	list, err := projects.List()
	if err != nil {
		return err
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(list)
	}

	if len(list) == 0 {
		fmt.Fprintln(os.Stdout, "(no projects registered yet — start a chat session in any directory to populate)")
		return nil
	}

	printProjectsTable(os.Stdout, list)
	return nil
}

// printProjectsTable formats the list as a 3-column table:
//
//	PATH   LAST USED    DATA DIR
//
// Width-adaptive: PATH column truncates from the LEFT (so trailing
// path component stays visible — matches renderHeaderBanner cwd
// truncation pattern).
func printProjectsTable(w *os.File, list []projects.Project) {
	now := time.Now().UTC()
	// Column widths
	pathW := len("PATH")
	for _, p := range list {
		if l := len(p.Path); l > pathW {
			pathW = l
		}
	}
	if pathW > 60 {
		pathW = 60
	}
	header := fmt.Sprintf("%-*s  %-12s  %s\n", pathW, "PATH", "LAST USED", "DATA DIR")
	fmt.Fprint(w, header)
	for _, p := range list {
		path := p.Path
		if len(path) > pathW {
			path = "…" + path[len(path)-pathW+1:]
		}
		fmt.Fprintf(w, "%-*s  %-12s  %s\n",
			pathW, path,
			humaniseAge(now.Sub(p.LastAccessed)),
			p.DataDir,
		)
	}
}

// humaniseAge turns a duration into a short human label. Mirrors the
// scale claude-code uses on its session picker — "2m ago", "3 days
// ago", "1 mo". Keeps the sessions column comfortably narrow.
func humaniseAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hr ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	default:
		months := int(d.Hours() / 24 / 30)
		return fmt.Sprintf("%d mo ago", months)
	}
}

// projectsAutoRegister is the side-effect hook the main command paths
// (chat, run, acp) call right after config.Load() succeeds. Failures
// are logged to stderr and swallowed — registry tracking should never
// block a real session.
func projectsAutoRegister(cwd, dataDir string) {
	if err := projects.Register(cwd, dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "metis: project registry update failed: %v\n", err)
	}
}

// Convenience for callers that have neither cwd nor home pre-resolved.
// Falls back silently when getwd fails (rare; metis would have bigger
// issues than registry tracking in that state).
func projectsAutoRegisterFromCWD(dataDir string) {
	cwd, err := os.Getwd()
	if err != nil || strings.TrimSpace(cwd) == "" {
		return
	}
	projectsAutoRegister(cwd, dataDir)
}
