package session

// recovery.go — crash-recovery pointer file written by every live
// metis session. Mirrors claude-code-sourcemap
// `restored-src/src/bridge/bridgePointer.ts`.
//
// Why: metis used to leave its session file half-written when the
// process died unclean (Ctrl+C of `metis run`, terminal closed during
// a long turn, kill -9, OS shutdown). The session JSONL was fine —
// each line is a discrete write — but the user had no easy way to
// know "session X was alive 90 seconds ago, here's how to resume."
//
// What: each running session writes a tiny pointer file the moment
// it boots:
//
//	~/.metis/session-pointers/<sha8(cwd)>.json
//
// Per-cwd because two concurrent metis terminals in different repos
// must not clobber each other (dev's primary use case). The cwd hash
// is the file key — short hex, no path-separator surprises on Windows.
//
// The file's MTIME is the freshness clock — periodic refreshes (60s
// ticker) bump mtime without changing content, so we don't burn write
// IOPS on a content diff just to record liveness. TTL is 30 minutes
// (shorter than claude-code's 4h: metis sessions are commonly
// minutes-long, half-hour stale is a strong "abandoned" signal).
//
// On clean exit metis calls ClearPointer; on next startup, if a
// fresh pointer exists the user sees:
//
//	"Found a recent session (PID X, started 4m ago) — `metis -c` to resume"
//
// Stale pointers (> TTL) get auto-cleared on read so they don't keep
// re-prompting after the user's already moved on.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PointerTTL caps how old a pointer can be before it's considered
// stale. 30 minutes balances "user came back from lunch — useful to
// resume" against "yesterday's crash — clutter."
const PointerTTL = 30 * time.Minute

// HeartbeatInterval is how often a live session re-touches its pointer
// to bump mtime. Must be < PointerTTL so a still-alive session is never
// mistaken for stale; 60s is also short enough that a crash inside
// the first heartbeat window still leaves a recent-enough pointer to
// be useful.
const HeartbeatInterval = 60 * time.Second

// Pointer is the JSON shape persisted to ~/.metis/session-pointers/<hash>.json.
// Embedded sessionID + cwd make ReadPointer self-describing — the
// file alone is enough to call `metis -r <id>` or `metis chat -W <cwd>`.
type Pointer struct {
	SessionID string    `json:"session_id"`
	CWD       string    `json:"cwd"`
	StartedAt time.Time `json:"started_at"`
	PID       int       `json:"pid"`
}

// AgeMs is exposed via ReadPointer's wrapper struct (so the caller
// doesn't have to compute time.Since).
type LivePointer struct {
	Pointer
	AgeMs int64 `json:"age_ms"`
}

// pointerDir returns ~/.metis/session-pointers/, creating it on first
// call. Mkdir is idempotent + cheap; we don't memoise.
func pointerDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".metis", "session-pointers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// pointerPath turns a cwd into the per-cwd pointer file path. SHA-256
// truncated to 8 hex chars: long enough that two real cwds never
// collide in practice (16M-key namespace), short enough that the
// filename stays scannable in `ls`.
func pointerPath(cwd string) (string, error) {
	dir, err := pointerDir()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd // best effort — exotic cwds get keyed by their literal form
	}
	sum := sha256.Sum256([]byte(abs))
	key := hex.EncodeToString(sum[:4]) // 8 hex chars
	return filepath.Join(dir, key+".json"), nil
}

// WritePointer creates / overwrites the pointer for cwd. Best-effort:
// a failed write must never crash the agent (a session lives just
// fine without crash-recovery state). Errors are returned for tests
// to assert against, but main.go ignores them intentionally.
func WritePointer(sessionID, cwd string) error {
	if sessionID == "" {
		return errors.New("session: empty sessionID")
	}
	path, err := pointerPath(cwd)
	if err != nil {
		return err
	}
	p := Pointer{
		SessionID: sessionID,
		CWD:       cwd,
		StartedAt: time.Now().UTC(),
		PID:       os.Getpid(),
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	// 0o600 because the cwd path can be sensitive (e.g. /Users/me/work/private-fork).
	return os.WriteFile(path, b, 0o600)
}

// RefreshPointer bumps the file's mtime so a long-running session
// stays "fresh" by the readers' clock. Cheaper than WritePointer
// (no marshal, no rewrite) — just os.Chtimes. If the file is missing
// (user manually deleted it, or filesystem GC'd) we re-create it
// from the supplied sessionID + cwd so heartbeats are self-healing.
func RefreshPointer(sessionID, cwd string) error {
	path, err := pointerPath(cwd)
	if err != nil {
		return err
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil {
		return nil
	}
	// Missing — re-create. (Anything other than ENOENT also lands
	// here; WritePointer's permission/marshal errors will surface.)
	return WritePointer(sessionID, cwd)
}

// ReadPointer returns the current pointer for cwd, or nil if there
// isn't one or it's stale. Stale pointers are deleted as a side
// effect so a subsequent startup doesn't keep prompting about the
// same dead session.
//
// Returns (nil, nil) for "no live pointer" — separate from (nil, err)
// for I/O errors. Callers typically ignore the err and just check the
// pointer.
func ReadPointer(cwd string) (*LivePointer, error) {
	path, err := pointerPath(cwd)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(path)
	if err != nil {
		// File missing is the common case; suppress.
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	age := time.Since(st.ModTime())
	if age > PointerTTL {
		_ = ClearPointer(cwd) // best effort
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Pointer
	if err := json.Unmarshal(raw, &p); err != nil {
		// Malformed — clear so we don't keep tripping on it.
		_ = ClearPointer(cwd)
		return nil, nil
	}
	return &LivePointer{Pointer: p, AgeMs: age.Milliseconds()}, nil
}

// ClearPointer deletes the pointer for cwd. Idempotent — ENOENT is
// expected when the previous shutdown was clean.
func ClearPointer(cwd string) error {
	path, err := pointerPath(cwd)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// StartHeartbeat spawns a goroutine that re-touches the pointer every
// HeartbeatInterval until ctx is cancelled. Returns immediately. Safe
// to call once per session boot — the goroutine cleans up on its own
// when ctx is done.
//
// On normal shutdown the caller should ALSO call ClearPointer (so the
// file disappears; otherwise it lingers until next startup notices
// it's stale). Heartbeat does NOT clear on its own — ctx cancel is
// not a "clean shutdown" signal (could be subprocess kill, etc.),
// and we want stale pointers to survive crashes.
func StartHeartbeat(ctx context.Context, sessionID, cwd string) {
	go func() {
		t := time.NewTicker(HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = RefreshPointer(sessionID, cwd)
			}
		}
	}()
}

// pointerMu serialises the rare external callers (metis ps / status
// inspectors) that want to scan all pointers — keeps directory reads
// race-free against the live writer's create.
var pointerMu sync.Mutex

// ListLivePointers returns every non-stale pointer across all cwds.
// Useful for `metis ps`-style listing and for "do I already have a
// session live in another terminal?" checks. Stale pointers are
// pruned as a side effect of reading.
func ListLivePointers() ([]LivePointer, error) {
	pointerMu.Lock()
	defer pointerMu.Unlock()
	dir, err := pointerDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []LivePointer
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		age := time.Since(st.ModTime())
		if age > PointerTTL {
			_ = os.Remove(path)
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var p Pointer
		if json.Unmarshal(raw, &p) != nil {
			_ = os.Remove(path)
			continue
		}
		out = append(out, LivePointer{Pointer: p, AgeMs: age.Milliseconds()})
	}
	return out, nil
}
