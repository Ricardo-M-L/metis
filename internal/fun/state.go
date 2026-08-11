package fun

// state.go — on-disk tracking for the /fun-managed media player. One
// state file per host (~/.metis/fun/music_state.json) because the
// player is a singleton — `/fun lofi` running twice in two metis
// sessions would just stack two background mpv's and double the
// audio. Single state record + "stop the old one before starting new"
// is the cleanest UX.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Ricardo-M-L/metis/internal/processutil"
)

// PlayerState is what `~/.metis/fun/music_state.json` contains. Kept
// minimal: PID for kill, URL/Title for "what's playing", started_at
// for "how long".
type PlayerState struct {
	PID       int       `json:"pid"`
	URL       string    `json:"url"`
	Title     string    `json:"title"` // "Lofi Girl 24/7" or "<search query>"
	StartedAt time.Time `json:"started_at"`
}

func stateFile() (string, error) {
	d, err := FunDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "music_state.json"), nil
}

// Save writes the state file atomically (write-temp + rename). Cheap
// because the payload is tiny.
func (s *PlayerState) Save() error {
	path, err := stateFile()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadState returns the tracked player or ErrNotRunning. Validates
// that the recorded PID still corresponds to a live process —
// crashed players or `kill -9 <pid>` from outside leave a stale file
// behind, and we don't want `/fun music status` to lie about it.
func LoadState() (*PlayerState, error) {
	path, err := stateFile()
	if err != nil {
		return nil, err
	}
	buf, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, ErrNotRunning
	}
	if err != nil {
		return nil, err
	}
	var s PlayerState
	if err := json.Unmarshal(buf, &s); err != nil {
		// Corrupt state file → treat as "nothing running" so the
		// next /fun lofi call can recover by overwriting.
		return nil, ErrNotRunning
	}
	if !pidAlive(s.PID) {
		// Stale — clean up so we don't keep failing the liveness
		// check forever.
		_ = os.Remove(path)
		return nil, ErrNotRunning
	}
	return &s, nil
}

// Clear removes the state file. Called after a successful Stop.
func ClearState() error {
	path, err := stateFile()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// pidAlive sends signal 0 — the POSIX "check existence" path. Works
// on macOS / Linux. ESRCH means no such process; EPERM means it
// exists but we don't own it (counts as alive — we recorded it, so
// we own it, but extra-paranoid case).
func pidAlive(pid int) bool { return processutil.Alive(pid) }

// FormatUptime returns a human-readable duration since the state was
// created, e.g. "2m 14s" or "1h 03m". Used by /fun music status.
func (s *PlayerState) FormatUptime() string {
	d := time.Since(s.StartedAt)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}
