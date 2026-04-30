package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/update"
	"github.com/Ricardo-M-L/metis/internal/version"
)

// maybeNotifyUpdate runs at TUI/REPL startup. It throttles to one network
// call per 24h and only prints a one-line notice — never auto-installs.
// Errors are swallowed silently; this should never block the user.
func maybeNotifyUpdate() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tag := update.MaybeCheck(ctx, config.Home(), version.Version)
	if tag == "" {
		return
	}
	fmt.Fprintf(os.Stderr,
		"\033[33m[update]\033[0m metis %s available (current: %s) — run `metis update` to install\n",
		strings.TrimPrefix(tag, "v"), strings.TrimPrefix(version.Version, "v"))
	update.MarkNotified(config.Home(), tag)
}

func cmdUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "Only check for a newer release; don't install")
	force := fs.Bool("force", false, "Reinstall even if already on the latest version")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: metis update [--check] [--force]

Self-update metis from the private GitHub release. Requires:
  METIS_GITHUB_TOKEN (or GITHUB_TOKEN) — PAT with read access to the repo

Flags:
  --check    Only check whether a newer release exists
  --force    Reinstall the latest release even if it matches the running version

Other env:
  METIS_REPO              Override repo (default: Ricardo-M-L/metis)
  METIS_NO_UPDATE_CHECK=1 Disable the daily startup check (does not affect this command)`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	token := update.Token()
	if token == "" {
		return errors.New(`no GitHub token set.

Set one of:
  export METIS_GITHUB_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxx
  export GITHUB_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxx

The token only needs read access to the metis repo (fine-grained PAT,
"Contents: Read-only" scope is enough).`)
	}

	cur := strings.TrimPrefix(version.Version, "v")

	rel, err := update.Latest(ctx, token)
	if err != nil {
		return fmt.Errorf("check latest release: %w", err)
	}
	latest := strings.TrimPrefix(rel.TagName, "v")

	switch {
	case *checkOnly:
		if update.IsNewer(cur, rel.TagName) {
			fmt.Printf("update available: %s -> %s\n", cur, latest)
			fmt.Printf("  release: %s\n", rel.HTMLURL)
			fmt.Printf("  run `metis update` to install\n")
			return nil
		}
		fmt.Printf("metis %s is up to date\n", cur)
		return nil
	case !*force && !update.IsNewer(cur, rel.TagName):
		fmt.Printf("metis %s is already the latest release\n", cur)
		fmt.Printf("(use --force to reinstall)\n")
		return nil
	}

	self, err := update.SelfPath()
	if err != nil {
		return fmt.Errorf("resolve self path: %w", err)
	}
	if err := update.CheckSelfPathSafe(self); err != nil {
		if errors.Is(err, update.ErrGoInstallManaged) {
			return fmt.Errorf(`%w

You appear to have installed metis with `+"`go install`"+`. To upgrade, run:

  go install github.com/Ricardo-M-L/metis/cmd/metis@%s`, err, rel.TagName)
		}
		return err
	}

	fmt.Printf("downloading metis %s for %s ...\n", latest, update.Target())
	if err := update.Apply(ctx, token, self, rel); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	fmt.Printf("installed metis %s at %s\n", latest, self)
	return nil
}
