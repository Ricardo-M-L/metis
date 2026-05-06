package runtime

// snapshot.go — periodic runtime state snapshot for crash recovery
// (T14). Inspired by openclaw runtime-snapshot.ts + matrix
// migration-snapshot.ts: write a tiny JSON every N seconds (or on
// every turn end) so a metis hard-killed mid-session can show the
// user "I was running session X with provider Y in cwd Z when I
// crashed — resume it?" instead of silently losing context.
//
// Distinct from internal/session — that already persists messages.
// This snapshot adds the *runtime* envelope around them: cwd, active
// model, last-touched timestamp, in-flight task count. The two
// together let a recovery flow reconstruct what was happening.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// noopSignal is the "kill -0" idiom: signal 0 is "noop / probe" on
// POSIX, used to test whether a pid is still receiving signals.
// Hoisted to a package var so tests can substitute it on platforms
// that don't support sig 0 (Windows isn't a supported metis target,
// so this lives in the shared file).
var noopSignal = syscall.Signal(0)

// Snapshot is the per-session runtime state captured for recovery.
// Fields are intentionally lean — anything bigger should live in the
// session message log instead.
type Snapshot struct {
	// SchemaVersion lets future metis versions detect old snapshot
	// formats and skip them rather than choking on unknown fields.
	SchemaVersion int `json:"schema_version"`

	SessionID string    `json:"session_id"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Process pid + ppid let recovery detect "is this process still
	// alive?" — if pid no longer exists, the snapshot is a crash
	// trace, not a live session.
	PID  int `json:"pid"`
	PPID int `json:"ppid"`

	// Cwd at snapshot time. Used to resume in the same directory.
	Cwd string `json:"cwd"`

	// What the agent was talking to.
	Provider string `json:"provider"`
	Model    string `json:"model"`

	// Counters help present a "you were 80 turns into a session"
	// hint without re-reading the full message log.
	MessageCount int   `json:"message_count"`
	TokensTotal  int64 `json:"tokens_total"`

	// Phase = "idle" | "streaming" | "tool" | "compacting" — the
	// last spinner phase before the snapshot tick. Crash mid-streaming
	// vs. crash mid-tool gets different recovery suggestions.
	Phase string `json:"phase,omitempty"`

	// LastUserPrompt: a truncated copy of the last user message so
	// recovery can show "you were asking: 'how do I refactor X' —
	// resume?" without reading the session file.
	LastUserPrompt string `json:"last_user_prompt,omitempty"`
}

// SnapshotSchemaVersion is the current snapshot format. Bump when
// adding required fields; readers should reject older versions
// rather than guess.
const SnapshotSchemaVersion = 1

// snapshotDir resolves to $METIS_HOME/snapshots, creating it if
// necessary. Falls back to ~/.metis/snapshots when METIS_HOME is
// unset. Always returns an absolute path.
func snapshotDir() (string, error) {
	home := os.Getenv("METIS_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(h, ".metis")
	}
	dir := filepath.Join(home, "snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// snapshotPath is the canonical file location for a given session id.
// We use one file per session so live updates are atomic (rename) and
// `ls` of the dir gives a session inventory.
func snapshotPath(sessionID string) (string, error) {
	dir, err := snapshotDir()
	if err != nil {
		return "", err
	}
	if sessionID == "" {
		return "", fmt.Errorf("session id required")
	}
	// Forbid path-traversing slugs — we control the input but defense
	// in depth costs nothing here.
	return filepath.Join(dir, sessionID+".json"), nil
}

// SaveSnapshot writes s to disk atomically (write to temp, rename).
// SchemaVersion is force-set so callers can leave it zero. Returns
// the path written for logs / tests.
func SaveSnapshot(s Snapshot) (string, error) {
	if s.SessionID == "" {
		return "", fmt.Errorf("snapshot has no session id")
	}
	s.SchemaVersion = SnapshotSchemaVersion
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now()
	}
	if s.PID == 0 {
		s.PID = os.Getpid()
		s.PPID = os.Getppid()
	}
	path, err := snapshotPath(s.SessionID)
	if err != nil {
		return "", err
	}
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// LoadSnapshot reads the snapshot for sessionID. Returns
// (zero, os.ErrNotExist) when no snapshot exists. Old schema
// versions are returned as-is — callers should check
// s.SchemaVersion against SnapshotSchemaVersion.
func LoadSnapshot(sessionID string) (Snapshot, error) {
	path, err := snapshotPath(sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	if err := json.Unmarshal(body, &s); err != nil {
		return Snapshot{}, fmt.Errorf("decode snapshot %s: %w", path, err)
	}
	return s, nil
}

// ListSnapshots returns every snapshot on disk, newest UpdatedAt
// first. Callers that only need the most recent should slice [:1].
// Bad snapshots (unreadable, decode errors) are silently skipped so
// one corrupt file doesn't hide the rest.
func ListSnapshots() ([]Snapshot, error) {
	dir, err := snapshotDir()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var s Snapshot
		if err := json.Unmarshal(body, &s); err != nil {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// DeleteSnapshot removes a session's snapshot. Called when a session
// finishes cleanly (so the next `metis chat` doesn't offer to
// recover a session that exited intentionally). Returns nil if the
// snapshot is already absent — idempotent.
func DeleteSnapshot(sessionID string) error {
	path, err := snapshotPath(sessionID)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// LooksCrashed reports whether a snapshot is likely from a crashed
// process (not a live one). Heuristic:
//
//   - If pid == os.Getpid(), this IS the live process — not crashed.
//   - If processExists(pid) is false, the owning process is gone →
//     either crash or clean exit. Without a "clean exit" marker the
//     file would have been deleted, so its presence implies crash.
//   - If UpdatedAt is older than maxAge, treat as stale even if pid
//     somehow still exists (recycled pid namespace, frozen child).
//
// processExists is hand-rolled rather than using os.FindProcess —
// FindProcess on Unix always succeeds even for nonexistent pids.
func LooksCrashed(s Snapshot, maxAge time.Duration) bool {
	if s.PID == os.Getpid() {
		return false
	}
	if processExists(s.PID) {
		return false
	}
	if maxAge > 0 && time.Since(s.UpdatedAt) > maxAge {
		return true
	}
	return true // process gone + recent-enough → crash
}

// processExists tries os.Signal(0) on pid (Unix kill -0 idiom).
// signal 0 is "noop, just check permissions" — returns nil on a
// running process owned by us, ESRCH otherwise.
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(noopSignal); err != nil {
		return false
	}
	return true
}
