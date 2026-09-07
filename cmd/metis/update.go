package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/update"
	"github.com/Ricardo-M-L/metis/internal/version"
)

const (
	autoUpdateInterval            = 30 * time.Minute
	autoUpdateHousekeepingTimeout = 5 * time.Second
)

// autoUpdateDependencies keeps the scheduler deterministic in tests. The
// production functions are assembled in defaultAutoUpdateDependencies; the
// loop itself never reaches out to global clocks or the network directly.
type autoUpdateDependencies struct {
	check          func(context.Context) string
	install        func(context.Context, string) (autoUpdateInstallResult, error)
	cleanupManaged func(context.Context)
	markNotified   func(string)
	notify         func(string)
	wait           func(context.Context, time.Duration) bool
}

type autoUpdateInstallResult struct {
	installed bool
	notice    string
}

type autoUpdaterStarter struct {
	once sync.Once
}

var processAutoUpdater autoUpdaterStarter

func defaultAutoUpdateDependencies() autoUpdateDependencies {
	return autoUpdateDependencies{
		check: func(ctx context.Context) string {
			return update.MaybeCheck(ctx, config.Home(), version.Version)
		},
		install: tryAutoInstall,
		cleanupManaged: func(ctx context.Context) {
			self, err := update.SelfPath()
			if err != nil {
				return
			}
			cleanupCtx, cancel := context.WithTimeout(ctx, autoUpdateHousekeepingTimeout)
			defer cancel()
			_ = update.CleanupManaged(cleanupCtx, self)
		},
		markNotified: func(tag string) {
			update.MarkNotified(config.Home(), tag)
		},
		wait: waitForAutoUpdate,
	}
}

// maybeAutoUpdate starts the process-lifetime updater and returns immediately.
// The string return is retained for compatibility with the existing chat
// startup call. The background goroutine never writes directly to the active
// Bubble Tea terminal; after an install, the next launch simply uses the new
// version. Network and install failures are silent. Disable the loop with
// METIS_NO_UPDATE_CHECK=1.
func maybeAutoUpdate(ctx context.Context) string {
	processAutoUpdater.start(ctx, defaultAutoUpdateDependencies())
	return ""
}

// start launches at most one updater loop without performing any network or
// filesystem work on the caller's goroutine. Keeping the Once on an instance
// makes the process singleton deterministic in tests without resetting a
// package global while another goroutine may still be alive.
func (s *autoUpdaterStarter) start(ctx context.Context, deps autoUpdateDependencies) bool {
	if os.Getenv("METIS_NO_UPDATE_CHECK") == "1" {
		return false
	}
	started := false
	s.once.Do(func() {
		started = true
		go runAutoUpdateLoop(ctx, deps)
	})
	return started
}

func runAutoUpdateLoop(ctx context.Context, deps autoUpdateDependencies) {
	handledTag := ""
	for {
		if ctx.Err() != nil {
			return
		}

		tag := ""
		if deps.check != nil {
			tag = deps.check(ctx)
		}
		// The running process still reports the version it started with after
		// the launcher is switched. Remember a handled tag so the 30-minute
		// check does not ask the core updater about it repeatedly.
		if tag != "" && tag != handledTag && deps.install != nil {
			result, err := deps.install(ctx, tag)
			if err == nil {
				// installed=false means another process won the shared lock and
				// already activated this tag. It is handled for this process, but
				// must not produce a false "installed" notification.
				handledTag = tag
				if result.installed {
					if deps.markNotified != nil {
						deps.markNotified(tag)
					}
					if result.notice != "" && deps.notify != nil {
						deps.notify(result.notice)
					}
				}
			}
		}
		// Cleanup is useful even when there is no new release: a version
		// protected by another process during the previous update becomes
		// eligible after that process exits.
		if deps.cleanupManaged != nil {
			deps.cleanupManaged(ctx)
		}

		if deps.wait == nil || !deps.wait(ctx, autoUpdateInterval) {
			return
		}
	}
}

func waitForAutoUpdate(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// tryAutoInstall downloads and installs the latest CLI stable release without user
// interaction. update.ApplyIfNeeded owns the cross-process install lock and
// the platform-specific atomic switch; that keeps automatic and manual
// updates on the same path, including Windows. The running process is left
// untouched and observes the new version only after restart.
func tryAutoInstall(ctx context.Context, tag string) (autoUpdateInstallResult, error) {
	token := update.Token()
	self, err := update.SelfPath()
	if err != nil {
		return autoUpdateInstallResult{}, err
	}
	if err := update.CheckSelfPathSafe(self); err != nil {
		return autoUpdateInstallResult{}, err
	}

	installCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	rel, err := update.Latest(installCtx, token)
	if err != nil {
		return autoUpdateInstallResult{}, err
	}
	// Discovery is repeated here after the initial notification check. A
	// changed channel must not downgrade this process or install a release
	// older than the one that triggered this attempt.
	if err := checkUpdateTarget(version.Version, rel.TagName, tag); err != nil {
		return autoUpdateInstallResult{}, err
	}
	if !update.IsNewer(version.Version, rel.TagName) {
		return autoUpdateInstallResult{}, nil
	}
	installed, err := update.ApplyIfNeeded(installCtx, token, self, rel)
	if err != nil {
		return autoUpdateInstallResult{}, err
	}
	if !installed {
		return autoUpdateInstallResult{}, nil
	}

	installedTag := rel.TagName
	if installedTag == "" {
		installedTag = tag
	}
	notice := fmt.Sprintf("metis %s installed (restart to apply)",
		strings.TrimPrefix(installedTag, "v"))
	return autoUpdateInstallResult{installed: true, notice: notice}, nil
}

// checkUpdateTarget protects the running process version before installation.
// The core updater separately rechecks the activated version under its lock,
// since another process may have upgraded the launcher in the meantime.
// announced is non-empty only for an automatic check-and-install pair.
func checkUpdateTarget(running, target, announced string) error {
	if update.IsNewer(target, running) {
		return fmt.Errorf("refusing to downgrade metis %s to CLI stable release %s; --force only permits reinstalling the same version", running, target)
	}
	if announced != "" && update.IsNewer(target, announced) {
		return fmt.Errorf("CLI stable release changed backwards from %s to %s; skipping this update attempt", announced, target)
	}
	return nil
}

func cmdUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "Only check for a newer CLI stable release; don't install")
	force := fs.Bool("force", false, "Allow reinstalling the same CLI stable version; never downgrade")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: metis update [--check] [--force]

Self-update metis from the public CLI stable releases on GitHub.
This channel is independent of the shared CLI/Desktop latest release.
No token is required.
METIS_GITHUB_TOKEN (or GITHUB_TOKEN) is optional for higher API rate limits.

Flags:
  --check    Only check whether a newer CLI stable release exists
  --force    Reinstall the CLI stable release if it matches the running version;
             never install a version older than the running version

Other env:
  METIS_REPO              Override repo (default: Ricardo-M-L/metis)
  METIS_NO_UPDATE_CHECK=1 Disable the interactive background update loop (does not affect this command)`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	token := update.Token()
	cur := strings.TrimPrefix(version.Version, "v")

	rel, err := update.Latest(ctx, token)
	if err != nil {
		return fmt.Errorf("check CLI stable release: %w", err)
	}
	latest := strings.TrimPrefix(rel.TagName, "v")

	if *checkOnly {
		if update.IsNewer(cur, rel.TagName) {
			fmt.Printf("update available: %s -> %s\n", cur, latest)
			fmt.Printf("  release: %s\n", rel.HTMLURL)
			fmt.Printf("  run `metis update` to install\n")
			return nil
		}
		if update.IsNewer(rel.TagName, cur) {
			fmt.Printf("metis %s is newer than the currently available CLI stable release %s; no downgrade will be installed\n", cur, latest)
			return nil
		}
		fmt.Printf("metis %s matches the currently available CLI stable release\n", cur)
		return nil
	}
	if err := checkUpdateTarget(cur, rel.TagName, ""); err != nil {
		return err
	}
	if !*force && !update.IsNewer(cur, rel.TagName) {
		fmt.Printf("metis %s matches the currently available CLI stable release\n", cur)
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
	fmt.Printf("installed metis %s; restart running sessions to use it\n", latest)
	warnIfGoBinShadows()
	return nil
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
