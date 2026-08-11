package memory

// dream_lock.go — cross-process advisory lock for the dreaming
// (auto-memory consolidation) workflow.
//
// metis runs as multiple short-lived CLI processes — `metis` in one
// terminal, `metis -r <sid>` in another, plus any scheduled
// `RemoteTrigger` invocations. Without a shared lock, every one of
// those processes would independently fire AutoMemoryExtractor at the
// end of its first turn, racing to write the same memdir files and
// burning duplicate LLM calls. The on-disk lock is what makes
// "≥6 h between dreams" durable across restarts and multi-window
// sessions.
//
// Design lifted from claude-code's services/autoDream/consolidationLock.ts:
//
//   - `.dream-lock` file in the memdir root.
//   - body = process PID (string), mtime = last successful completion.
//   - TryAcquire writes our PID then re-reads to verify it stuck
//     (POSIX guarantees the write is atomic, but two near-simultaneous
//     writers can both think they won — the re-read resolves it: only
//     the process whose PID is still on disk after a brief delay holds
//     the lock).
//   - Release leaves the file in place with mtime=now so the time-gate
//     uses a real "last completed at" timestamp.
//   - Rollback restores the previous mtime (Chtimes) on failure so the
//     next process's time-gate fires immediately — we don't want a
//     crashed dream to lock everyone out for 6 h.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/processutil"
)

// DreamLockFilename is the per-memdir lock file name. Hidden so it
// doesn't clutter `ls ~/.metis/memory/`.
const DreamLockFilename = ".dream-lock"

// DreamLock is a cross-process advisory lock around the dreaming
// workflow. Construct via NewDreamLock; methods are safe to call from
// multiple goroutines in the same process (the underlying file ops are
// the synchronization).
type DreamLock struct {
	path string
}

// NewDreamLock returns a lock anchored at <memdirRoot>/.dream-lock.
// memdirRoot is expected to already exist (memdir.EnsureRoot has run);
// TryAcquire will fail with a clear error otherwise.
func NewDreamLock(memdirRoot string) *DreamLock {
	return &DreamLock{path: filepath.Join(memdirRoot, DreamLockFilename)}
}

// Path is the absolute path to the lock file. Exposed for tests and
// logs — production code should use the LastSuccessAt / TryAcquire /
// Release / Rollback methods.
func (d *DreamLock) Path() string { return d.path }

// LastSuccessAt returns the mtime of the lock file IF the most recent
// holder completed cleanly. If the lock body still contains a PID
// (Release was never called — process crashed or was SIGKILL'd
// mid-dream) AND that PID is no longer alive, we treat the lock as
// stale and return zero, letting the next dream proceed.
//
// Why this matters: a 60s dream that hits the 120s waitForkInflight
// cap exits with a non-empty PID body. Naïvely trusting the mtime
// would make every future LoopEnd see "dream completed at <crash
// time>" and block for the full 6 h interval. The PID-alive check
// rescues the next process from this trap. Empty body (the post-
// Release state) is the unambiguous "completed cleanly" signal and
// always trusts mtime.
//
// Returns zero on stat error (file doesn't exist, EPERM) — callers
// treat zero as "infinitely long ago" so a fresh install still
// dreams when other gates clear.
func (d *DreamLock) LastSuccessAt() time.Time {
	info, err := os.Stat(d.path)
	if err != nil {
		return time.Time{}
	}
	body, err := os.ReadFile(d.path)
	if err != nil || len(strings.TrimSpace(string(body))) == 0 {
		// Empty body = Release fired = clean completion. Trust mtime.
		return info.ModTime()
	}
	// Body has a PID — check if the process is still alive.
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		// Malformed body — be conservative and trust mtime (treats as
		// in-flight, gate refuses). A malformed lock is a config bug
		// the user should `rm ~/.metis/memory/.dream-lock` to clear.
		return info.ModTime()
	}
	if pidAlive(pid) {
		// Another process is still dreaming — gate must refuse.
		return info.ModTime()
	}
	// Stale crashed lock — let the next dream through.
	return time.Time{}
}

// pidAlive returns true when process `pid` is reachable on this
// host. POSIX: kill(pid, 0) returns 0 if the signal could have been
// delivered (process exists, even zombie). ESRCH means the PID is
// gone. Other errors (EPERM, etc.) mean the process exists but we
// can't signal it — still "alive" for our purpose.
func pidAlive(pid int) bool { return processutil.Alive(pid) }

// TryAcquire writes our PID into the lock file and re-reads to verify
// no other process clobbered us. Returns:
//
//	priorMtime — the mtime BEFORE we wrote, so Rollback can restore it
//	             on dream-failure (keeps the next caller's time-gate
//	             from being incorrectly satisfied by a failed run).
//	ok         — true when we hold the lock. False means another
//	             process beat us to the write; caller should bail.
//	err        — non-nil on filesystem errors (memdir missing, EPERM).
//	             ok will be false. Callers should log but not panic —
//	             a missing memdir is a config issue, not a dream issue.
//
// Why re-read instead of O_CREATE|O_EXCL: claude-code chose this path
// to keep the lock self-healing — if a process crashes mid-dream, the
// next process's TryAcquire just overwrites the stale PID and proceeds
// (no manual cleanup needed). The PID verification step ensures we
// catch the rare double-write race; flock(2) would also work but is
// non-portable across the Linux/macOS/WSL surfaces metis supports.
func (d *DreamLock) TryAcquire() (priorMtime time.Time, ok bool, err error) {
	// Capture prior mtime BEFORE writing — we need to know what to
	// restore if the dream fails downstream.
	priorMtime = d.LastSuccessAt()

	pid := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(d.path, []byte(pid), 0o644); err != nil {
		return priorMtime, false, fmt.Errorf("dream-lock: write %s: %w", d.path, err)
	}

	// Brief settle window — give the kernel a chance to flush so a
	// second writer's stat sees our content. 20ms is comfortable on
	// every FS that backs ~/.metis (APFS, ext4, ZFS, NTFS) and gives
	// the concurrent-acquire test enough headroom under CI load
	// (5ms was racy on contended GitHub-Actions runners). We don't
	// loop because spinning here would defeat the purpose of an
	// advisory lock — at most one of two simultaneous winners
	// proceeds. 20ms × ~1 dream / 6 h = 0.0001% wall-clock overhead.
	time.Sleep(20 * time.Millisecond)

	got, err := os.ReadFile(d.path)
	if err != nil {
		return priorMtime, false, fmt.Errorf("dream-lock: verify %s: %w", d.path, err)
	}
	if strings.TrimSpace(string(got)) != pid {
		// Another process won. Don't roll back — they own the lock
		// now and will set their own mtime when they finish.
		return priorMtime, false, nil
	}
	return priorMtime, true, nil
}

// Release marks the dream as completed successfully. The mtime is
// already "now" from TryAcquire's write — Release just truncates the
// PID body so a subsequent reader can distinguish "completed cleanly"
// from "crashed mid-dream with PID still present". Errors are
// non-fatal: even if the truncate fails the mtime is correct, which
// is the load-bearing part for the time gate.
func (d *DreamLock) Release() error {
	if err := os.WriteFile(d.path, []byte{}, 0o644); err != nil {
		return fmt.Errorf("dream-lock: release %s: %w", d.path, err)
	}
	return nil
}

// Rollback restores the lock's mtime to priorMtime so the next process
// sees the previous "last successful at" timestamp — that way a
// failed dream doesn't reset the 6 h timer and trap dreaming for
// another full window. Pass the value returned by TryAcquire.
//
// If priorMtime is zero (lock didn't exist before our acquire), we
// remove the file entirely so the next caller's LastSuccessAt also
// reports zero. errors.Is(err, os.ErrNotExist) is silently ignored —
// the file already being gone is fine.
func (d *DreamLock) Rollback(priorMtime time.Time) error {
	if priorMtime.IsZero() {
		if err := os.Remove(d.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("dream-lock: rollback-remove %s: %w", d.path, err)
		}
		return nil
	}
	if err := os.Chtimes(d.path, priorMtime, priorMtime); err != nil {
		return fmt.Errorf("dream-lock: rollback-chtimes %s: %w", d.path, err)
	}
	return nil
}
