package update

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	installLockPoll       = 50 * time.Millisecond
	staleLockArtifactAge  = 24 * time.Hour
	maxRunningLockFileLen = 64 * 1024
	installLockPending    = "install.lock.d.pending."
	installReclaimPrefix  = "install.reclaim."
)

type installLockOwner struct {
	PID       int    `json:"pid"`
	Nonce     string `json:"nonce"`
	CreatedAt int64  `json:"created_at"`
}

type reclaimGuardOwner struct {
	PID         int    `json:"pid"`
	Nonce       string `json:"nonce"`
	TargetPID   int    `json:"target_pid"`
	TargetNonce string `json:"target_nonce"`
	CreatedAt   int64  `json:"created_at"`
}

type runningLockOwner struct {
	PID       int    `json:"pid"`
	Nonce     string `json:"nonce"`
	Version   string `json:"version"`
	ExecPath  string `json:"exec_path"`
	CreatedAt int64  `json:"created_at"`
}

type processRegistration struct {
	nonce string
	refs  int
}

var runningRegistrations = struct {
	sync.Mutex
	items map[string]*processRegistration
}{items: make(map[string]*processRegistration)}

// These hooks make lock publication/reclamation races deterministic in tests.
// Production leaves both nil.
var (
	installLockPendingReadyForTest     func(string)
	installLockBeforeStaleClaimForTest func()
	reclaimGuardBeforeRetireForTest    func()
)

// RegisterRunningVersion records the managed version used by this process.
// CleanupManaged protects every version whose registered PID is still alive.
// Non-managed binaries (including go-install development builds) are a safe
// no-op and do not create a share/metis tree next to an arbitrary executable.
func RegisterRunningVersion(destPath, runningVersion string) (func(), error) {
	runningExecutable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return registerRunningVersion(destPath, runningVersion, runningExecutable)
}

func registerRunningVersion(destPath, runningVersion, runningExecutable string) (func(), error) {
	_, layout, ok := managedLayout(destPath)
	if !ok {
		return func() {}, nil
	}
	version, err := normalizeVersion(runningVersion)
	if err != nil {
		return func() {}, nil
	}
	versioned := versionBinary(layout, version)
	// Registration describes the actual executing image, not whatever version
	// the launcher happens to target now. This closes the race where another
	// updater switches current before a just-started old process registers.
	if !runningExecutableMatchesVersion(runningExecutable, layout, versioned) {
		return func() {}, nil
	}
	for _, dir := range []string{layout.managedRoot, layout.locksRoot, layout.runningLocksRoot, filepath.Join(layout.runningLocksRoot, version)} {
		if err := ensureDirectDirectory(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create running-version lock directory: %w", err)
		}
	}
	dir := filepath.Join(layout.runningLocksRoot, version)
	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".json")

	runningRegistrations.Lock()
	if existing := runningRegistrations.items[path]; existing != nil {
		existing.refs++
		nonce := existing.nonce
		runningRegistrations.Unlock()
		return runningRelease(path, nonce), nil
	}
	nonce, err := randomNonce()
	if err != nil {
		runningRegistrations.Unlock()
		return nil, err
	}
	owner := runningLockOwner{
		PID:       os.Getpid(),
		Nonce:     nonce,
		Version:   version,
		ExecPath:  versioned,
		CreatedAt: time.Now().Unix(),
	}
	b, err := json.Marshal(owner)
	if err == nil {
		err = writeFileAtomic(path, b, 0o600)
	}
	if err != nil {
		runningRegistrations.Unlock()
		return nil, fmt.Errorf("register running version: %w", err)
	}
	runningRegistrations.items[path] = &processRegistration{nonce: nonce, refs: 1}
	runningRegistrations.Unlock()
	return runningRelease(path, nonce), nil
}

func runningRelease(path, nonce string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			runningRegistrations.Lock()
			reg := runningRegistrations.items[path]
			if reg == nil || reg.nonce != nonce {
				runningRegistrations.Unlock()
				return
			}
			reg.refs--
			if reg.refs > 0 {
				runningRegistrations.Unlock()
				return
			}
			delete(runningRegistrations.items, path)
			runningRegistrations.Unlock()

			var current runningLockOwner
			if err := readJSONFile(path, &current); err == nil && current.Nonce == nonce {
				_ = os.Remove(path)
				_ = os.Remove(filepath.Dir(path))
			}
		})
	}
}

// CleanupManaged performs best-effort housekeeping even when there is no new
// release. It shares the same global lock as Apply so it cannot prune a version
// while another process is staging or activating an update.
func CleanupManaged(ctx context.Context, destPath string) error {
	_, layout, ok := managedLayout(destPath)
	if !ok {
		return nil
	}
	release, err := acquireInstallLock(ctx, layout)
	if err != nil {
		return err
	}
	defer release()
	return cleanupManagedLocked(layout, time.Now())
}

// managedLayout accepts only a verifiably managed launcher or immutable
// version path. It is deliberately stricter than managedLayoutForCleanup:
// registration must never create state for a legacy/unmanaged flat binary.
func managedLayout(path string) (string, installLayout, bool) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", installLayout{}, false
	}
	if launcher, ok := managedLauncherForExecutable(path); ok {
		return launcher, layoutForLauncher(launcher), true
	}
	// path may already be the stable launcher. resolveCurrentVersion verifies
	// the symlink/current-version metadata and immutable binary.
	layout := layoutForLauncher(path)
	if _, ok := resolveCurrentVersion(layout); ok {
		return path, layout, true
	}
	return "", installLayout{}, false
}

// managedLayoutForApply also accepts a missing or legacy flat launcher
// because first migration needs to create the managed tree. A path that is
// itself a managed immutable binary is normalized back to its launcher; it is
// never treated as the place to install another versions tree.
func managedLayoutForApply(path string) (string, installLayout, bool) {
	path, err := filepath.Abs(path)
	if err != nil || path == "" {
		return "", installLayout{}, false
	}
	if launcher, ok := managedLauncherForExecutable(path); ok {
		return launcher, layoutForLauncher(launcher), true
	}
	if _, looksManaged := launcherFromVersionExecutableShape(path); looksManaged {
		return "", installLayout{}, false
	}
	return path, layoutForLauncher(path), true
}

func acquireInstallLock(ctx context.Context, layout installLayout) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, dir := range []string{layout.managedRoot, layout.locksRoot} {
		if err := ensureDirectDirectory(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create update locks directory: %w", err)
		}
	}
	for {
		lockInfo, lstatErr := os.Lstat(layout.installLockDir)
		if errors.Is(lstatErr, os.ErrNotExist) {
			release, acquired, err := publishInstallLock(layout)
			if err != nil {
				return nil, err
			}
			if acquired {
				return release, nil
			}
			continue
		}
		if lstatErr != nil || !lockInfo.IsDir() || lockInfo.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("update lock path is not a direct regular directory: %s", layout.installLockDir)
		}

		owner, ownerErr := readInstallLockOwner(layout.installLockDir)
		stale := false
		switch {
		case ownerErr == nil:
			alive, known := processAlive(owner.PID)
			stale = known && !alive
		case errors.Is(ownerErr, os.ErrNotExist), errors.Is(ownerErr, io.EOF):
			// A compliant updater publishes a fully-owned pending directory, so a
			// fixed ownerless lock came from an interrupted legacy/script install
			// or manual modification. It is ambiguous and requires manual recovery.
			stale = false
		default:
			// A malformed owner is ambiguous. Never steal it based on age: the
			// owning process may still be downloading a large release.
			stale = false
		}
		if stale {
			if err := quarantineInstallLock(layout.installLockDir, owner); err == nil {
				continue
			}
		}

		timer := time.NewTimer(installLockPoll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// publishInstallLock first writes a complete owner into a unique directory,
// then atomically renames it into the fixed path without replacement. This
// eliminates the fixed-path mkdir -> owner-write window and makes cleanup safe:
// failure can remove only the unique pending directory created by this call.
func publishInstallLock(layout installLayout) (func(), bool, error) {
	nonce, err := randomNonce()
	if err != nil {
		return nil, false, err
	}
	pending := filepath.Join(layout.locksRoot, installLockPending+nonce)
	if err := os.Mkdir(pending, 0o700); err != nil {
		return nil, false, fmt.Errorf("create pending update lock: %w", err)
	}
	claimed := false
	defer func() {
		if !claimed {
			removeLockArtifact(pending)
		}
	}()

	owner := installLockOwner{PID: os.Getpid(), Nonce: nonce, CreatedAt: time.Now().Unix()}
	if err := writeInstallLockOwner(pending, owner); err != nil {
		return nil, false, fmt.Errorf("write pending update lock owner: %w", err)
	}
	if installLockPendingReadyForTest != nil {
		installLockPendingReadyForTest(pending)
	}
	if err := renameDirNoReplace(pending, layout.installLockDir); err != nil {
		if _, lstatErr := os.Lstat(layout.installLockDir); lstatErr == nil {
			return nil, false, nil
		} else if !errors.Is(lstatErr, os.ErrNotExist) {
			return nil, false, fmt.Errorf("inspect contended update lock: %w", lstatErr)
		}
		return nil, false, fmt.Errorf("publish update lock: %w", err)
	}
	claimed = true
	return installLockRelease(layout.installLockDir, nonce), true, nil
}

func writeInstallLockOwner(lockDir string, owner installLockOwner) error {
	return writeJSONOwnerExclusive(lockDir, owner)
}

func writeJSONOwnerExclusive(lockDir string, owner any) error {
	b, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(lockDir, lockOwnerFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func installLockRelease(lockDir, nonce string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			owner, err := readInstallLockOwner(lockDir)
			if err != nil || owner.Nonce != nonce || owner.PID != os.Getpid() {
				return
			}
			quarantine := lockDir + ".release." + nonce
			if err := os.Rename(lockDir, quarantine); err == nil {
				_ = os.RemoveAll(quarantine)
			}
		})
	}
}

func quarantineInstallLock(lockDir string, expected installLockOwner) error {
	if expected.PID <= 0 || !validInstallLockNonce(expected.Nonce) {
		return fmt.Errorf("invalid stale update lock owner")
	}
	current, err := readInstallLockOwner(lockDir)
	if err != nil || current.Nonce != expected.Nonce || current.PID != expected.PID {
		return fmt.Errorf("update lock owner changed")
	}
	if installLockBeforeStaleClaimForTest != nil {
		installLockBeforeStaleClaimForTest()
	}

	// Serialize every reclaimer that observed this owner. The guard is itself
	// atomically published and safely recoverable after its owner dies.
	releaseGuard, err := acquireReclaimGuard(lockDir, expected)
	if err != nil {
		return err
	}
	defer releaseGuard()
	current, err = readInstallLockOwner(lockDir)
	if err != nil || current.Nonce != expected.Nonce || current.PID != expected.PID {
		return fmt.Errorf("update lock owner changed")
	}
	alive, known := processAlive(current.PID)
	if !known || alive {
		return fmt.Errorf("update lock owner is alive or cannot be verified")
	}
	nonce, err := randomNonce()
	if err != nil {
		return err
	}
	quarantine := lockDir + ".stale." + nonce
	if err := os.Rename(lockDir, quarantine); err != nil {
		return err
	}
	return os.RemoveAll(quarantine)
}

func reclaimGuardPath(lockDir string, target installLockOwner) string {
	return filepath.Join(filepath.Dir(lockDir), installReclaimPrefix+target.Nonce+".d")
}

func acquireReclaimGuard(lockDir string, target installLockOwner) (func(), error) {
	if target.PID <= 0 || !validInstallLockNonce(target.Nonce) {
		return nil, fmt.Errorf("invalid reclaim target owner")
	}
	guardPath := reclaimGuardPath(lockDir, target)
	for {
		info, err := os.Lstat(guardPath)
		if errors.Is(err, os.ErrNotExist) {
			release, acquired, publishErr := publishReclaimGuard(guardPath, target)
			if publishErr != nil {
				return nil, publishErr
			}
			if acquired {
				return release, nil
			}
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("reclaim guard is not a direct regular directory: %s", guardPath)
		}
		owner, ownerErr := readReclaimGuardOwner(guardPath)
		if ownerErr != nil || owner.TargetPID != target.PID || owner.TargetNonce != target.Nonce {
			// Ownerless, malformed, and mismatched guards are ambiguous. Never
			// recover them based on age; they require manual inspection.
			return nil, fmt.Errorf("reclaim guard owner cannot be verified: %s", guardPath)
		}
		alive, known := processAlive(owner.PID)
		if !known || alive {
			return nil, fmt.Errorf("another process is reclaiming the update lock (pid %d)", owner.PID)
		}
		if err := retireReclaimGuard(guardPath, owner); err != nil {
			return nil, err
		}
	}
}

func publishReclaimGuard(guardPath string, target installLockOwner) (func(), bool, error) {
	nonce, err := randomNonce()
	if err != nil {
		return nil, false, err
	}
	pending := guardPath + ".pending." + nonce
	if err := os.Mkdir(pending, 0o700); err != nil {
		return nil, false, fmt.Errorf("create pending reclaim guard: %w", err)
	}
	claimed := false
	defer func() {
		if !claimed {
			removeLockArtifact(pending)
		}
	}()
	owner := reclaimGuardOwner{
		PID:         os.Getpid(),
		Nonce:       nonce,
		TargetPID:   target.PID,
		TargetNonce: target.Nonce,
		CreatedAt:   time.Now().Unix(),
	}
	if err := writeJSONOwnerExclusive(pending, owner); err != nil {
		return nil, false, fmt.Errorf("write pending reclaim guard owner: %w", err)
	}
	if err := renameDirNoReplace(pending, guardPath); err != nil {
		if _, lstatErr := os.Lstat(guardPath); lstatErr == nil {
			return nil, false, nil
		} else if !errors.Is(lstatErr, os.ErrNotExist) {
			return nil, false, fmt.Errorf("inspect contended reclaim guard: %w", lstatErr)
		}
		return nil, false, fmt.Errorf("publish reclaim guard: %w", err)
	}
	claimed = true
	return reclaimGuardRelease(guardPath, owner), true, nil
}

func retireReclaimGuard(guardPath string, expected reclaimGuardOwner) error {
	current, err := readReclaimGuardOwner(guardPath)
	if err != nil || !sameReclaimGuardOwner(current, expected) {
		return fmt.Errorf("reclaim guard owner changed")
	}
	if reclaimGuardBeforeRetireForTest != nil {
		reclaimGuardBeforeRetireForTest()
	}
	retired := guardPath + ".retired." + expected.Nonce
	// The retired destination is deliberately never reused or time-pruned.
	// Any stale observer of this guard owner must contend for this same path;
	// once it exists, no-replace prevents that observer from moving a successor.
	if err := renameDirNoReplace(guardPath, retired); err != nil {
		return fmt.Errorf("retire stale reclaim guard: %w", err)
	}
	return nil
}

func reclaimGuardRelease(guardPath string, expected reclaimGuardOwner) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			current, err := readReclaimGuardOwner(guardPath)
			if err != nil || !sameReclaimGuardOwner(current, expected) {
				return
			}
			released := guardPath + ".release." + expected.Nonce
			if err := renameDirNoReplace(guardPath, released); err == nil {
				removeLockArtifact(released)
			}
		})
	}
}

func readReclaimGuardOwner(guardPath string) (reclaimGuardOwner, error) {
	var owner reclaimGuardOwner
	if err := readJSONFile(filepath.Join(guardPath, lockOwnerFile), &owner); err != nil {
		return owner, err
	}
	if owner.PID <= 0 || owner.TargetPID <= 0 || owner.CreatedAt <= 0 ||
		!validInstallLockNonce(owner.Nonce) || !validInstallLockNonce(owner.TargetNonce) {
		return reclaimGuardOwner{}, fmt.Errorf("invalid reclaim guard owner")
	}
	return owner, nil
}

func sameReclaimGuardOwner(a, b reclaimGuardOwner) bool {
	return a.PID == b.PID && a.Nonce == b.Nonce && a.TargetPID == b.TargetPID && a.TargetNonce == b.TargetNonce
}

func readInstallLockOwner(lockDir string) (installLockOwner, error) {
	var owner installLockOwner
	err := readJSONFile(filepath.Join(lockDir, lockOwnerFile), &owner)
	if err != nil {
		return owner, err
	}
	if owner.PID <= 0 || owner.CreatedAt <= 0 || !validInstallLockNonce(owner.Nonce) {
		return installLockOwner{}, fmt.Errorf("invalid update lock owner")
	}
	return owner, nil
}

func validInstallLockNonce(nonce string) bool {
	if nonce == "" || len(nonce) > 128 || nonce == "." || nonce == ".." {
		return false
	}
	for _, r := range nonce {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func readJSONFile(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(io.LimitReader(f, maxRunningLockFileLen)).Decode(dst)
}

func cleanupManagedLocked(layout installLayout, now time.Time) error {
	cleanupStaging(layout.stagingRoot, now.Add(-stagingMaxAge))
	cleanupPlatformTemps(layout, now.Add(-stagingMaxAge).UnixNano())
	cleanupStaleLockArtifacts(layout.locksRoot, now.Add(-staleLockArtifactAge))

	protected, runningCertain := collectRunningVersions(layout)
	current, currentOK := resolveCurrentVersion(layout)
	if !currentOK || !runningCertain {
		// If current/launcher resolution is uncertain, retaining everything is
		// safer than deleting a binary that may still be selected externally.
		return nil
	}
	protected[current] = struct{}{}

	type candidate struct {
		version string
		path    string
		mtime   time.Time
	}
	rootInfo, rootErr := os.Lstat(layout.versionsRoot)
	if errors.Is(rootErr, os.ErrNotExist) {
		return nil
	}
	if rootErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	entries, err := os.ReadDir(layout.versionsRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	candidates := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		version, err := normalizeVersion(entry.Name())
		if err != nil || version != entry.Name() {
			continue
		}
		path := filepath.Join(layout.versionsRoot, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		bin := filepath.Join(path, executableName())
		binInfo, err := os.Lstat(bin)
		if err != nil || !binInfo.Mode().IsRegular() || binInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if _, keep := protected[version]; keep {
			continue
		}
		candidates = append(candidates, candidate{version: version, path: path, mtime: binInfo.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].mtime.Equal(candidates[j].mtime) {
			return candidates[i].version > candidates[j].version
		}
		return candidates[i].mtime.After(candidates[j].mtime)
	})
	if len(candidates) <= retainedOldVersions {
		return nil
	}
	for _, old := range candidates[retainedOldVersions:] {
		// Re-lstat immediately before RemoveAll. RemoveAll itself does not
		// traverse a path that has become a symlink; it unlinks that symlink.
		info, err := os.Lstat(old.path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := os.RemoveAll(old.path); err != nil {
			return fmt.Errorf("remove old version %s: %w", old.version, err)
		}
		_ = os.RemoveAll(filepath.Join(layout.runningLocksRoot, old.version))
	}
	return nil
}

func collectRunningVersions(layout installLayout) (map[string]struct{}, bool) {
	protected := make(map[string]struct{})
	rootInfo, rootErr := os.Lstat(layout.runningLocksRoot)
	if errors.Is(rootErr, os.ErrNotExist) {
		return protected, true
	}
	if rootErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return protected, false
	}
	versionDirs, err := os.ReadDir(layout.runningLocksRoot)
	if errors.Is(err, os.ErrNotExist) {
		return protected, true
	}
	if err != nil {
		return protected, false
	}
	for _, versionEntry := range versionDirs {
		version, versionErr := normalizeVersion(versionEntry.Name())
		path := filepath.Join(layout.runningLocksRoot, versionEntry.Name())
		info, lstatErr := os.Lstat(path)
		if versionErr != nil || version != versionEntry.Name() {
			continue
		}
		if lstatErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			protected[version] = struct{}{}
			continue
		}
		locks, readErr := os.ReadDir(path)
		if readErr != nil {
			protected[version] = struct{}{}
			continue
		}
		keepVersion := false
		for _, lockEntry := range locks {
			lockPath := filepath.Join(path, lockEntry.Name())
			lockInfo, err := os.Lstat(lockPath)
			if err != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode()&os.ModeSymlink != 0 {
				keepVersion = true
				continue
			}
			var owner runningLockOwner
			if err := readJSONFile(lockPath, &owner); err != nil || owner.Version != version || owner.PID <= 0 || owner.Nonce == "" {
				keepVersion = true
				continue
			}
			wantName := strconv.Itoa(owner.PID) + ".json"
			if lockEntry.Name() != wantName {
				keepVersion = true
				continue
			}
			alive, known := processAlive(owner.PID)
			if !known || alive {
				keepVersion = true
				continue
			}
			_ = os.Remove(lockPath)
		}
		if keepVersion {
			protected[version] = struct{}{}
		} else {
			_ = os.Remove(path)
		}
	}
	return protected, true
}

func cleanupStaging(root string, cutoff time.Time) {
	rootInfo, rootErr := os.Lstat(root)
	if rootErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			_ = os.Remove(path)
			continue
		}
		_ = os.RemoveAll(path)
	}
}

func cleanupStaleLockArtifacts(root string, cutoff time.Time) {
	rootInfo, rootErr := os.Lstat(root)
	if rootErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), installLockPending) && !strings.HasPrefix(entry.Name(), "install.lock.d.stale.") && !strings.HasPrefix(entry.Name(), "install.lock.d.release.") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err == nil && info.ModTime().Before(cutoff) {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				_ = os.Remove(path)
			} else {
				_ = os.RemoveAll(path)
			}
		}
	}
}

func removeLockArtifact(path string) {
	info, err := os.Lstat(path)
	if err != nil {
		return
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		_ = os.Remove(path)
		return
	}
	_ = os.RemoveAll(path)
}

func randomNonce() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate update lock nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := ensureDirectDirectory(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	nonce, err := randomNonce()
	if err != nil {
		return err
	}
	if info, lstatErr := os.Lstat(path); lstatErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to replace non-regular managed file: %s", path)
		}
	} else if !errors.Is(lstatErr, os.ErrNotExist) {
		return lstatErr
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+nonce+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := replaceFileAtomic(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func ensureDirectDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, mode); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to use symlink or non-directory as managed directory: %s", path)
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}
