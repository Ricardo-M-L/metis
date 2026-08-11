package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	gort "runtime"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/update"
	"github.com/Ricardo-M-L/metis/internal/version"
)

// maybeAutoUpdate runs at TUI/REPL startup. It throttles release checks and,
// when a newer release exists, downloads and installs it on supported Unix
// silently in the background. Errors are swallowed; this should never block
// the user.
//
// Returns the formatted notice string (empty when up-to-date, check errored,
// or auto-update is disabled). Caller surfaces the notice. For TUI we stash
// it via tui.SetPendingUpdateNotice so it lands as an info row inside
// alt-screen.
//
// Disable with METIS_NO_UPDATE_CHECK=1.
func maybeAutoUpdate() string {
	if os.Getenv("METIS_NO_UPDATE_CHECK") == "1" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tag := update.MaybeCheck(ctx, config.Home(), version.Version)
	if tag == "" {
		return ""
	}
	update.MarkNotified(config.Home(), tag)

	// Attempt silent auto-install. If it fails (go-install managed,
	// network error, etc.), fall back to the manual notice.
	if notice := tryAutoInstall(tag); notice != "" {
		return notice
	}

	var notice string
	if gort.GOOS == "windows" {
		notice = fmt.Sprintf("metis %s available (current: %s) — install with: %s",
			strings.TrimPrefix(tag, "v"), strings.TrimPrefix(version.Version, "v"), windowsInstallCommand(tag))
	} else {
		notice = fmt.Sprintf("metis %s available (current: %s) — run `metis update` to install",
			strings.TrimPrefix(tag, "v"), strings.TrimPrefix(version.Version, "v"))
	}
	fmt.Fprintf(os.Stderr, "\033[33m[update]\033[0m %s\n", notice)
	return notice
}

// tryAutoInstall attempts to download and install the given tag without
// user interaction. Returns a notice string on success, empty on failure.
//
// Locking: ~/.metis/.update.lock prevents concurrent auto-installs across
// metis processes. The lock is best-effort — failure to acquire just skips
// this round.
func tryAutoInstall(tag string) string {
	// Windows cannot atomically replace a running .exe and the Unix updater
	// uses a symlink-based version farm. Keep the background check enabled,
	// but leave installation to the signed/checksummed PowerShell installer.
	if gort.GOOS == "windows" {
		return ""
	}
	token := update.Token()

	// Best-effort lock: create exclusively, 5-minute expiry.
	lockPath := filepath.Join(config.Home(), ".update.lock")
	if f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err != nil {
		// Lock held by another process — check if it's stale.
		if info, err2 := os.Stat(lockPath); err2 == nil && time.Since(info.ModTime()) > 5*time.Minute {
			_ = os.Remove(lockPath)
		} else {
			return ""
		}
	} else {
		f.Close()
		defer os.Remove(lockPath)
	}

	self, err := update.SelfPath()
	if err != nil {
		return ""
	}
	if err := update.CheckSelfPathSafe(self); err != nil {
		// go-install managed or unsafe path — skip auto-install.
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rel, err := update.Latest(ctx, token)
	if err != nil {
		return ""
	}
	if err := update.Apply(ctx, token, self, rel); err != nil {
		return ""
	}

	notice := fmt.Sprintf("metis %s installed (restart to apply)",
		strings.TrimPrefix(rel.TagName, "v"))
	fmt.Fprintf(os.Stderr, "\033[32m[update]\033[0m %s\n", notice)
	return notice
}

func cmdUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "Only check for a newer release; don't install")
	force := fs.Bool("force", false, "Reinstall even if already on the latest version")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: metis update [--check] [--force]

Self-update metis from the public GitHub release. No token is required.
METIS_GITHUB_TOKEN (or GITHUB_TOKEN) is optional for higher API rate limits.

Flags:
  --check    Only check whether a newer release exists
  --force    Reinstall the latest release even if it matches the running version

Other env:
  METIS_REPO              Override repo (default: Ricardo-M-L/metis)
  METIS_NO_UPDATE_CHECK=1 Disable the throttled startup check (does not affect this command)`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	token := update.Token()
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
			if gort.GOOS == "windows" {
				fmt.Printf("  install with: %s\n", windowsInstallCommand(rel.TagName))
			} else {
				fmt.Printf("  run `metis update` to install\n")
			}
			return nil
		}
		fmt.Printf("metis %s is up to date\n", cur)
		return nil
	case !*force && !update.IsNewer(cur, rel.TagName):
		fmt.Printf("metis %s is already the latest release\n", cur)
		fmt.Printf("(use --force to reinstall)\n")
		return nil
	}

	if gort.GOOS == "windows" {
		return fmt.Errorf(`automatic replacement of a running Windows executable is not supported

Install %s with the checksummed PowerShell installer:

  %s`, latest, windowsInstallCommand(rel.TagName))
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
	versionedBin := filepath.Join(filepath.Dir(filepath.Dir(self)), "share", "metis", "versions", latest, "metis")
	fmt.Printf("installed metis %s → %s (symlink → %s)\n", latest, self, versionedBin)
	warnIfGoBinShadows()
	return nil
}

func windowsInstallCommand(tag string) string {
	return fmt.Sprintf("$env:METIS_VERSION='%s'; irm https://raw.githubusercontent.com/Ricardo-M-L/metis/%s/install/install.ps1 | iex", tag, tag)
}

// warnIfGoBinShadows prints a heads-up when a second metis binary exists
// under $GOBIN/$GOPATH/bin. If that directory precedes ~/.local/bin in
// PATH, the stale go-install binary shadows the freshly self-updated one
// and the user never sees the new version (verify report 2026-08-05:
// "PATH-shadowing after migration gets no user-facing warning").
func warnIfGoBinShadows() {
	gobin := os.Getenv("GOBIN")
	if gobin == "" {
		if gopath := os.Getenv("GOPATH"); gopath != "" {
			gobin = filepath.Join(gopath, "bin")
		} else if home, err := os.UserHomeDir(); err == nil {
			gobin = filepath.Join(home, "go", "bin")
		}
	}
	if gobin == "" {
		return
	}
	stale := filepath.Join(gobin, "metis")
	if _, err := os.Stat(stale); err != nil {
		return
	}
	fmt.Printf("\nwarning: a second metis binary exists at %s\n", stale)
	fmt.Printf("if `%s` appears before the self-update directory in PATH, the stale binary will shadow this install — delete it:\n", gobin)
	fmt.Printf("  rm %s\n", stale)
}
