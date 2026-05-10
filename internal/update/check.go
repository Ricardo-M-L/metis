package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// stateFile lives under the user's metis config dir and remembers when
	// we last successfully checked + the latest tag we saw.
	stateFile = ".update-check"
	// minInterval throttles the background daily check.
	minInterval = 24 * time.Hour
)

type checkState struct {
	LastCheck   time.Time `json:"last_check"`
	LatestTag   string    `json:"latest_tag,omitempty"`
	LastTagSeen string    `json:"last_tag_seen,omitempty"` // last tag we *notified* about
	LastNotify  time.Time `json:"last_notify,omitempty"`
}

func statePath(home string) string { return filepath.Join(home, stateFile) }

// latestVersionFile is the simple one-line cache the TUI's chrome row
// reads to render "current: X · latest: Y". Lives next to the JSON
// state because the TUI doesn't import internal/update — keeping the
// path side-by-side avoids cross-cutting it through another package.
const latestVersionFile = "latest_version"

// writeLatestVersionFile mirrors a successful release fetch into a
// plain-text file the TUI reads (renderVersionLine in
// internal/tui/render_chrome.go::readLatestVersion). Best-effort: a
// write failure is silent because the JSON state file is the
// authoritative cache; this file is purely a render-side hint.
func writeLatestVersionFile(home, tag string) {
	if home == "" || tag == "" {
		return
	}
	_ = os.MkdirAll(home, 0o755)
	_ = os.WriteFile(filepath.Join(home, latestVersionFile), []byte(tag+"\n"), 0o644)
}

func loadState(path string) checkState {
	b, err := os.ReadFile(path)
	if err != nil {
		return checkState{}
	}
	var s checkState
	if err := json.Unmarshal(b, &s); err != nil {
		return checkState{}
	}
	return s
}

func saveState(path string, s checkState) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o644)
}

// MaybeCheck is the daily-throttled version notifier intended for chat
// startup. It returns the latest available tag if one is strictly newer than
// `currentVersion`; "" otherwise (also returns "" on any error so callers can
// ignore failures silently).
//
// Skipped entirely when:
//   - METIS_NO_UPDATE_CHECK=1 is set
//   - no token is available
//   - we already checked within minInterval
//
// configHome is the metis config dir (e.g. ~/.metis); the throttle state
// file lives inside it.
func MaybeCheck(ctx context.Context, configHome, currentVersion string) string {
	if os.Getenv("METIS_NO_UPDATE_CHECK") == "1" {
		return ""
	}
	token := Token()
	if token == "" {
		return ""
	}
	sp := statePath(configHome)
	st := loadState(sp)
	if !st.LastCheck.IsZero() && time.Since(st.LastCheck) < minInterval {
		// Within throttle window — refresh the TUI hint file from cache
		// so the chrome row keeps showing "latest" even when we don't
		// hit the network this startup. Without this, users who delete
		// ~/.metis/latest_version (or upgrade past it) lose the hint
		// for up to 24h until the next throttle expiry.
		writeLatestVersionFile(configHome, st.LatestTag)
		// Within throttle window — but if we already saw a newer tag and
		// haven't notified the user about *this* version yet, surface it.
		if st.LatestTag != "" && IsNewer(currentVersion, st.LatestTag) && st.LastTagSeen != st.LatestTag {
			return st.LatestTag
		}
		return ""
	}

	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	r, err := Latest(cctx, token)
	if err != nil {
		// Don't update LastCheck on failure — try again next startup.
		return ""
	}
	st.LastCheck = time.Now()
	st.LatestTag = r.TagName
	saveState(sp, st)
	// Mirror the freshly-seen tag into ~/.metis/latest_version so the
	// TUI's chrome row picks it up on its next render. Without this the
	// "current: X · latest: Y" hint never lights up even after a
	// successful network check (image #2/#3 user feedback 2026-05-10).
	writeLatestVersionFile(configHome, r.TagName)

	if IsNewer(currentVersion, r.TagName) {
		return r.TagName
	}
	return ""
}

// MarkNotified records that we've shown the user a notice about `tag` so we
// don't nag them on every startup until a newer release lands.
func MarkNotified(configHome, tag string) {
	sp := statePath(configHome)
	st := loadState(sp)
	st.LastTagSeen = tag
	st.LastNotify = time.Now()
	saveState(sp, st)
}

// SelfPath returns the absolute path to the running metis binary.
func SelfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// EvalSymlinks fails on platforms / paths where it can't stat;
		// fall back to the raw exe path.
		return exe, nil
	}
	return resolved, nil
}

// ErrGoInstallManaged is returned when SelfPath points at $GOBIN/$GOPATH/bin,
// which means the binary was installed via `go install` — self-update would
// silently shadow that managed install.
var ErrGoInstallManaged = errors.New("binary appears to be managed by `go install` — use `go install ...@latest` instead")

// CheckSelfPathSafe sanity-checks the running binary path before we attempt
// to overwrite it.
func CheckSelfPathSafe(path string) error {
	if path == "" {
		return fmt.Errorf("could not resolve self path")
	}
	// Refuse to clobber a Go-managed install — different upgrade story.
	gobin := os.Getenv("GOBIN")
	if gobin == "" {
		if gopath := os.Getenv("GOPATH"); gopath != "" {
			gobin = filepath.Join(gopath, "bin")
		} else if home, err := os.UserHomeDir(); err == nil {
			gobin = filepath.Join(home, "go", "bin")
		}
	}
	if gobin != "" {
		abs, err := filepath.Abs(path)
		if err == nil {
			gabs, _ := filepath.Abs(gobin)
			if filepath.Dir(abs) == gabs {
				return ErrGoInstallManaged
			}
		}
	}
	return nil
}
