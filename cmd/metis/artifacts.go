package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/Ricardo-M-L/metis/internal/artifact"
)

const artifactsHelp = `metis artifacts — Manage durable local HTML artifacts

Usage:
  metis artifacts list --session ID [--json]
  metis artifacts show <id> --session ID [--json]
  metis artifacts create <file.html|-> --session ID [--title TITLE] [--json]
  metis artifacts update <id> <file.html|-> --session ID [--title TITLE] [--json]
  metis artifacts export <id> --out PATH --session ID [--version N] [--json]
  metis artifacts delete <id> --yes --session ID [--json]

Use '-' as the create/update input path to read HTML from stdin.
The singular 'metis artifact' form is an alias.
`

type artifactCommandStore interface {
	Create(sessionID, title, rawHTML string) (*artifact.Manifest, error)
	Update(sessionID, id, title, rawHTML string) (*artifact.Manifest, error)
	List(sessionID string) ([]artifact.Manifest, error)
	Get(sessionID, id string) (*artifact.Manifest, error)
	Export(sessionID, id string, version int, destination string) error
	Delete(sessionID, id string) error
}

type artifactStoreAdapter struct{ store *artifact.Store }

func (a artifactStoreAdapter) Create(sessionID, title, rawHTML string) (*artifact.Manifest, error) {
	return a.store.Create(sessionID, title, rawHTML)
}

func (a artifactStoreAdapter) Update(sessionID, id, title, rawHTML string) (*artifact.Manifest, error) {
	return a.store.Update(sessionID, id, title, rawHTML)
}

func (a artifactStoreAdapter) List(sessionID string) ([]artifact.Manifest, error) {
	return a.store.List(sessionID)
}

func (a artifactStoreAdapter) Get(sessionID, id string) (*artifact.Manifest, error) {
	return a.store.Get(sessionID, id)
}

func (a artifactStoreAdapter) Export(sessionID, id string, version int, destination string) error {
	return a.store.Export(sessionID, id, version, destination)
}

// Delete stays on this adapter so the command layer has one narrow interface.
// The shared artifact store owns validation, locking, and recursive cleanup.
func (a artifactStoreAdapter) Delete(sessionID, id string) error {
	return a.store.Delete(sessionID, id)
}

func newArtifactCommandStore() (artifactCommandStore, error) {
	store, err := artifact.DefaultStore()
	if err != nil {
		return nil, err
	}
	return artifactStoreAdapter{store: store}, nil
}

func cmdArtifacts(args []string) error {
	store, err := newArtifactCommandStore()
	if err != nil {
		return err
	}
	return runArtifactCommand(args, os.Stdin, os.Stdout, store)
}

type artifactCommandOptions struct {
	action      string
	jsonOutput  bool
	yes         bool
	sessionID   string
	title       string
	outPath     string
	version     int
	positionals []string
}

func runArtifactCommand(args []string, stdin io.Reader, stdout io.Writer, store artifactCommandStore) error {
	opts, err := parseArtifactCommand(args)
	if err != nil {
		return err
	}
	if opts.action == "help" {
		_, err := io.WriteString(stdout, artifactsHelp)
		return err
	}
	if store == nil {
		return errors.New("artifacts: store unavailable")
	}

	switch opts.action {
	case "list":
		items, err := store.List(opts.sessionID)
		if err != nil {
			return err
		}
		if opts.jsonOutput {
			return writeArtifactJSON(stdout, items)
		}
		return printArtifactList(stdout, items)

	case "show":
		item, err := ownedArtifact(store, opts.positionals[0], opts.sessionID)
		if err != nil {
			return err
		}
		if opts.jsonOutput {
			return writeArtifactJSON(stdout, item)
		}
		return printArtifact(stdout, item)

	case "create":
		html, err := readArtifactInput(opts.positionals[0], stdin)
		if err != nil {
			return err
		}
		title := strings.TrimSpace(opts.title)
		if title == "" {
			title = artifactTitleFromPath(opts.positionals[0])
		}
		item, err := store.Create(opts.sessionID, title, html)
		if err != nil {
			return err
		}
		return printArtifactMutation(stdout, opts.jsonOutput, "created", item)

	case "update":
		html, err := readArtifactInput(opts.positionals[1], stdin)
		if err != nil {
			return err
		}
		item, err := store.Update(opts.sessionID, opts.positionals[0], opts.title, html)
		if err != nil {
			return err
		}
		return printArtifactMutation(stdout, opts.jsonOutput, "updated", item)

	case "export":
		item, err := ownedArtifact(store, opts.positionals[0], opts.sessionID)
		if err != nil {
			return err
		}
		destination, err := filepath.Abs(opts.outPath)
		if err != nil {
			return fmt.Errorf("artifacts export: resolve destination: %w", err)
		}
		if err := store.Export(item.SessionID, item.ID, opts.version, destination); err != nil {
			return err
		}
		if opts.jsonOutput {
			return writeArtifactJSON(stdout, map[string]any{
				"exported": true, "id": item.ID, "version": selectedArtifactVersion(item, opts.version), "path": destination,
			})
		}
		_, err = fmt.Fprintf(stdout, "exported %s version %d to %s\n", item.ID, selectedArtifactVersion(item, opts.version), destination)
		return err

	case "delete":
		item, err := ownedArtifact(store, opts.positionals[0], opts.sessionID)
		if err != nil {
			return err
		}
		if err := store.Delete(item.SessionID, item.ID); err != nil {
			return err
		}
		if opts.jsonOutput {
			return writeArtifactJSON(stdout, map[string]any{"deleted": true, "id": item.ID})
		}
		_, err = fmt.Fprintf(stdout, "deleted artifact %s\n", item.ID)
		return err
	}
	return fmt.Errorf("artifacts: unsupported action %q", opts.action)
}

func parseArtifactCommand(args []string) (artifactCommandOptions, error) {
	opts := artifactCommandOptions{action: "list"}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		opts.action = strings.ToLower(strings.TrimSpace(args[0]))
		args = args[1:]
	}
	switch opts.action {
	case "-h", "--help", "help":
		opts.action = "help"
		return opts, nil
	case "list", "show", "create", "update", "export", "delete":
	default:
		return opts, fmt.Errorf("artifacts: unknown action %q (want list|show|create|update|export|delete)", opts.action)
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			opts.positionals = append(opts.positionals, args[i+1:]...)
			i = len(args)
		case arg == "--json":
			opts.jsonOutput = true
		case arg == "--yes" || arg == "-y":
			opts.yes = true
		case arg == "--help" || arg == "-h":
			opts.action = "help"
			return opts, nil
		case arg == "--session" || arg == "--title" || arg == "--out" || arg == "-o" || arg == "--version":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("artifacts %s: %s requires a value", opts.action, arg)
			}
			i++
			if err := setArtifactOption(&opts, arg, args[i]); err != nil {
				return opts, err
			}
		case strings.HasPrefix(arg, "--session="):
			opts.sessionID = strings.TrimSpace(strings.TrimPrefix(arg, "--session="))
		case strings.HasPrefix(arg, "--title="):
			opts.title = strings.TrimPrefix(arg, "--title=")
		case strings.HasPrefix(arg, "--out="):
			opts.outPath = strings.TrimPrefix(arg, "--out=")
		case strings.HasPrefix(arg, "--version="):
			if err := setArtifactOption(&opts, "--version", strings.TrimPrefix(arg, "--version=")); err != nil {
				return opts, err
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			return opts, fmt.Errorf("artifacts %s: unknown option %q", opts.action, arg)
		default:
			opts.positionals = append(opts.positionals, arg)
		}
	}

	if err := validateArtifactCommand(opts); err != nil {
		return opts, err
	}
	return opts, nil
}

func setArtifactOption(opts *artifactCommandOptions, name, value string) error {
	switch name {
	case "--session":
		opts.sessionID = strings.TrimSpace(value)
	case "--title":
		opts.title = value
	case "--out", "-o":
		opts.outPath = value
	case "--version":
		version, err := strconv.Atoi(value)
		if err != nil || version < 1 {
			return fmt.Errorf("artifacts %s: version must be a positive integer", opts.action)
		}
		opts.version = version
	}
	return nil
}

func validateArtifactCommand(opts artifactCommandOptions) error {
	wantPositionals := map[string]int{"list": 0, "show": 1, "create": 1, "update": 2, "export": 1, "delete": 1}
	if got, want := len(opts.positionals), wantPositionals[opts.action]; got != want {
		return fmt.Errorf("artifacts %s: expected %d argument(s), got %d\n%s", opts.action, want, got, artifactsHelp)
	}
	if opts.sessionID == "" {
		return fmt.Errorf("artifacts %s: --session ID is required", opts.action)
	}
	if opts.action == "export" && strings.TrimSpace(opts.outPath) == "" {
		return errors.New("artifacts export: --out PATH is required")
	}
	if opts.action == "delete" && !opts.yes {
		return errors.New("artifacts delete: refusing permanent deletion without --yes")
	}
	if opts.version != 0 && opts.action != "export" {
		return fmt.Errorf("artifacts %s: --version is only valid for export", opts.action)
	}
	if opts.outPath != "" && opts.action != "export" {
		return fmt.Errorf("artifacts %s: --out is only valid for export", opts.action)
	}
	if opts.title != "" && opts.action != "create" && opts.action != "update" {
		return fmt.Errorf("artifacts %s: --title is only valid for create or update", opts.action)
	}
	if opts.yes && opts.action != "delete" {
		return fmt.Errorf("artifacts %s: --yes is only valid for delete", opts.action)
	}
	return nil
}

func readArtifactInput(path string, stdin io.Reader) (string, error) {
	var reader io.Reader
	var file *os.File
	if path == "-" {
		if stdin == nil {
			return "", errors.New("artifacts: stdin unavailable")
		}
		reader = stdin
	} else {
		var err error
		file, err = os.Open(path)
		if err != nil {
			return "", fmt.Errorf("artifacts: read %s: %w", path, err)
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return "", fmt.Errorf("artifacts: inspect %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("artifacts: input %s is not a regular file", path)
		}
		reader = file
	}
	body, err := io.ReadAll(io.LimitReader(reader, artifact.MaxHTMLBytes+1))
	if err != nil {
		return "", fmt.Errorf("artifacts: read HTML: %w", err)
	}
	if len(body) > artifact.MaxHTMLBytes {
		return "", artifact.ErrTooLarge
	}
	return string(body), nil
}

func artifactTitleFromPath(path string) string {
	if path == "-" {
		return "Untitled artifact"
	}
	base := filepath.Base(path)
	title := strings.TrimSuffix(base, filepath.Ext(base))
	if strings.TrimSpace(title) == "" {
		return "Untitled artifact"
	}
	return title
}

func ownedArtifact(store artifactCommandStore, id, sessionID string) (*artifact.Manifest, error) {
	return store.Get(sessionID, id)
}

func selectedArtifactVersion(item *artifact.Manifest, requested int) int {
	if requested > 0 {
		return requested
	}
	return item.CurrentVersion
}

func writeArtifactJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func printArtifactMutation(w io.Writer, jsonOutput bool, verb string, item *artifact.Manifest) error {
	if item == nil || len(item.Versions) == 0 {
		return errors.New("artifacts: store returned an incomplete manifest")
	}
	version := item.Versions[len(item.Versions)-1]
	if jsonOutput {
		return writeArtifactJSON(w, map[string]any{"artifact": item, "version": version})
	}
	_, err := fmt.Fprintf(w, "%s artifact %s version %d (%s)\n", verb, item.ID, version.Number, item.Title)
	return err
}

func printArtifactList(w io.Writer, items []artifact.Manifest) error {
	if len(items) == 0 {
		_, err := io.WriteString(w, "(no artifacts)\n")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tTITLE\tVERSION\tSESSION\tUPDATED"); err != nil {
		return err
	}
	for _, item := range items {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", item.ID, item.Title, item.CurrentVersion, item.SessionID, item.UpdatedAt.Format("2006-01-02 15:04:05Z07:00")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func printArtifact(w io.Writer, item *artifact.Manifest) error {
	if item == nil {
		return artifact.ErrNotFound
	}
	if _, err := fmt.Fprintf(w, "id: %s\nsession: %s\ntitle: %s\ntype: %s\ncurrent version: %d\ncreated: %s\nupdated: %s\nversions:\n",
		item.ID, item.SessionID, item.Title, item.MIMEType, item.CurrentVersion,
		item.CreatedAt.Format("2006-01-02 15:04:05Z07:00"), item.UpdatedAt.Format("2006-01-02 15:04:05Z07:00")); err != nil {
		return err
	}
	for _, version := range item.Versions {
		if _, err := fmt.Fprintf(w, "  %d  %d bytes  %s  %s\n", version.Number, version.Size, version.SHA256, version.CreatedAt.Format("2006-01-02 15:04:05Z07:00")); err != nil {
			return err
		}
	}
	return nil
}
