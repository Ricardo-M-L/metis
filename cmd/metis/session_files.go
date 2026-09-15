package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles"
)

const sessionFilesHelp = `metis files — Read session-generated local files

Usage:
  metis files list --session ID [--json] [--sessions-dir PATH]
  metis files show <id> --session ID [--json] [--sessions-dir PATH]

Uses METIS_HOME/sessions (default ~/.metis/sessions). For a custom session
store, pass --sessions-dir. This command does not load provider credentials,
start an agent, or modify files. Previews are bounded, read-only, and redacted.
`

type sessionFilesCommandStore interface {
	List(string) ([]sessionfiles.File, error)
	Read(string, string) (*sessionfiles.Preview, error)
}

type sessionFilesCommandOptions struct {
	action      string
	sessionID   string
	sessionsDir string
	jsonOutput  bool
	positionals []string
}

func cmdSessionFiles(args []string) error {
	opts, err := parseSessionFilesCommand(args)
	if err != nil {
		return err
	}
	if opts.action == "help" {
		_, err = io.WriteString(os.Stdout, sessionFilesHelp)
		return err
	}
	service, err := sessionFilesCommandService(opts)
	if err != nil {
		return err
	}
	return runSessionFilesCommand(args, os.Stdout, service)
}

func sessionFilesCommandService(opts sessionFilesCommandOptions) (*sessionfiles.Service, error) {
	dir := opts.sessionsDir
	if dir == "" {
		dir = filepath.Join(config.Home(), "sessions")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, errors.New("files: cannot resolve session directory")
	}
	// Store.Load is read-only. NewStore and config.Load intentionally are not
	// used here: they can initialize or migrate user state on a first run.
	return sessionfiles.New(&session.Store{Dir: dir}), nil
}

func runSessionFilesCommand(args []string, stdout io.Writer, store sessionFilesCommandStore) error {
	opts, err := parseSessionFilesCommand(args)
	if err != nil {
		return err
	}
	if opts.action == "help" {
		_, err = io.WriteString(stdout, sessionFilesHelp)
		return err
	}
	if store == nil {
		return errors.New("files: session store unavailable")
	}
	if opts.action == "list" {
		files, err := store.List(opts.sessionID)
		if err != nil {
			return err
		}
		if opts.jsonOutput {
			return json.NewEncoder(stdout).Encode(map[string]any{"files": files})
		}
		if len(files) == 0 {
			_, err = io.WriteString(stdout, "No previewable files in this session.\n")
			return err
		}
		table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		if _, err = fmt.Fprintln(table, "ID\tKIND\tSOURCE\tPATH"); err != nil {
			return err
		}
		for _, file := range files {
			if _, err = fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", file.ID, file.Kind, file.Source, file.Path); err != nil {
				return err
			}
		}
		return table.Flush()
	}
	preview, err := store.Read(opts.sessionID, opts.positionals[0])
	if err != nil {
		return err
	}
	if opts.jsonOutput {
		return json.NewEncoder(stdout).Encode(preview)
	}
	if _, err = io.WriteString(stdout, preview.Content); err != nil {
		return err
	}
	if !strings.HasSuffix(preview.Content, "\n") {
		if _, err = io.WriteString(stdout, "\n"); err != nil {
			return err
		}
	}
	if preview.Truncated {
		_, err = io.WriteString(stdout, "\n[Preview truncated at 512 KiB]\n")
	}
	return err
}

func parseSessionFilesCommand(args []string) (sessionFilesCommandOptions, error) {
	opts := sessionFilesCommandOptions{action: "list"}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		opts.action = args[0]
		args = args[1:]
	}
	if opts.action == "help" {
		return opts, nil
	}
	if opts.action != "list" && opts.action != "show" {
		return opts, errors.New("files: unknown action (want list or show)")
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--help" || arg == "-h":
			opts.action = "help"
			return opts, nil
		case arg == "--json":
			opts.jsonOutput = true
		case arg == "--session" || arg == "--sessions-dir":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-") {
				return opts, fmt.Errorf("files: %s requires a value", arg)
			}
			i++
			if arg == "--session" {
				opts.sessionID = args[i]
			} else {
				opts.sessionsDir = args[i]
			}
		case strings.HasPrefix(arg, "--session="):
			opts.sessionID = strings.TrimPrefix(arg, "--session=")
		case strings.HasPrefix(arg, "--sessions-dir="):
			opts.sessionsDir = strings.TrimPrefix(arg, "--sessions-dir=")
			if strings.TrimSpace(opts.sessionsDir) == "" {
				return opts, errors.New("files: --sessions-dir requires a value")
			}
		case strings.HasPrefix(arg, "-"):
			return opts, errors.New("files: unknown option")
		default:
			opts.positionals = append(opts.positionals, arg)
		}
	}
	if strings.TrimSpace(opts.sessionID) == "" {
		return opts, errors.New("files: --session ID is required")
	}
	if opts.action == "list" && len(opts.positionals) != 0 || opts.action == "show" && len(opts.positionals) != 1 {
		return opts, errors.New("usage: metis files list --session ID | metis files show <id> --session ID")
	}
	return opts, nil
}
