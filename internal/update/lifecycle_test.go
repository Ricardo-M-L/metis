package update

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func seedManagedVersion(t *testing.T, versionsRoot, version string, modified time.Time) string {
	t.Helper()
	dir := filepath.Join(versionsRoot, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir version %s: %v", version, err)
	}
	bin := filepath.Join(dir, executableName())
	if err := os.WriteFile(bin, []byte("metis "+version), 0o755); err != nil {
		t.Fatalf("write version %s: %v", version, err)
	}
	if err := os.Chtimes(bin, modified, modified); err != nil {
		t.Fatalf("chtimes binary %s: %v", version, err)
	}
	if err := os.Chtimes(dir, modified, modified); err != nil {
		t.Fatalf("chtimes directory %s: %v", version, err)
	}
	return bin
}

func replaceLauncherSymlink(t *testing.T, launcher, target string) {
	t.Helper()
	tmp := launcher + ".test-link"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		t.Fatalf("symlink %s: %v", target, err)
	}
	if err := os.Rename(tmp, launcher); err != nil {
		t.Fatalf("replace launcher: %v", err)
	}
}

func TestCleanupManagedProtectsCurrentAndRunningAndRetainsTwoOld(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix launcher layout test")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	now := time.Now()
	paths := map[string]string{}
	for i := 1; i <= 5; i++ {
		version := "0.0." + strconv.Itoa(i)
		paths[version] = seedManagedVersion(t, layout.versionsRoot, version, now.Add(time.Duration(i)*time.Minute))
	}

	// Simulate an old v1 process registering after another process has already
	// atomically advanced the launcher to v5.
	replaceLauncherSymlink(t, launcher, paths["0.0.5"])
	releaseRunning, err := registerRunningVersion(launcher, "0.0.1", paths["0.0.1"])
	if err != nil {
		t.Fatalf("RegisterRunningVersion: %v", err)
	}
	t.Cleanup(releaseRunning)

	if err := CleanupManaged(context.Background(), launcher); err != nil {
		t.Fatalf("CleanupManaged: %v", err)
	}

	// v5 is current, v1 is running, and the two newest otherwise-unprotected
	// old versions (v4 and v3) are retained. Only v2 is eligible for deletion.
	for _, version := range []string{"0.0.1", "0.0.3", "0.0.4", "0.0.5"} {
		if _, err := os.Stat(paths[version]); err != nil {
			t.Errorf("protected/retained version %s removed: %v", version, err)
		}
	}
	if _, err := os.Lstat(filepath.Dir(paths["0.0.2"])); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old version 0.0.2 still exists, err=%v", err)
	}
}

func TestRunningVersionReleaseIsIdempotent(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix launcher fixture")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	current := seedManagedVersion(t, layout.versionsRoot, "1.0.0", time.Now())
	replaceLauncherSymlink(t, launcher, current)
	release, err := registerRunningVersion(launcher, "1.0.0", current)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(layout.runningLocksRoot, "1.0.0", strconv.Itoa(os.Getpid())+".json")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("running lock missing: %v", err)
	}
	release()
	release() // main's explicit os.Exit cleanup plus deferred cleanup
	if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("double release left or corrupted lock: %v", err)
	}
}

func TestCleanupManagedIsNoopForUnmanagedFlatBinary(t *testing.T) {
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", executableName())
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("legacy or go-install binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	if err := CleanupManaged(context.Background(), launcher); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(layout.managedRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CleanupManaged created state for unmanaged binary: err=%v", err)
	}
}

func TestMalformedRunningLockProtectsItsVersion(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix launcher layout test")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	now := time.Now()
	paths := make(map[string]string)
	for i := 1; i <= 5; i++ {
		version := "1.0." + strconv.Itoa(i)
		paths[version] = seedManagedVersion(t, layout.versionsRoot, version, now.Add(time.Duration(i)*time.Minute))
	}
	replaceLauncherSymlink(t, launcher, paths["1.0.5"])
	lockDir := filepath.Join(layout.runningLocksRoot, "1.0.1")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "unknown.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CleanupManaged(context.Background(), launcher); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths["1.0.1"]); err != nil {
		t.Fatalf("ambiguous running lock did not protect version: %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(paths["1.0.2"])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old unprotected version remains, err=%v", err)
	}
}

func TestCleanupManagedDoesNothingWhenCurrentCannotBeResolved(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix launcher layout test")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("legacy flat install"), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	old := seedManagedVersion(t, layout.versionsRoot, "0.0.1", time.Now().Add(-24*time.Hour))
	seedManagedVersion(t, layout.versionsRoot, "0.0.2", time.Now().Add(-23*time.Hour))
	seedManagedVersion(t, layout.versionsRoot, "0.0.3", time.Now().Add(-22*time.Hour))
	seedManagedVersion(t, layout.versionsRoot, "0.0.4", time.Now().Add(-21*time.Hour))

	if err := CleanupManaged(context.Background(), launcher); err != nil {
		t.Fatalf("CleanupManaged: %v", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("cleanup pruned with an uncertain current version: %v", err)
	}
}

func TestCleanupManagedNeverFollowsVersionSymlink(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix symlink safety test")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	current := seedManagedVersion(t, layout.versionsRoot, "9.0.0", time.Now())
	replaceLauncherSymlink(t, launcher, current)

	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(sentinel, []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(layout.versionsRoot, "0.0.1")
	if err := os.Symlink(filepath.Dir(sentinel), link); err != nil {
		t.Fatal(err)
	}
	// Add enough real old versions that cleanup definitely performs pruning.
	for i := 2; i <= 5; i++ {
		seedManagedVersion(t, layout.versionsRoot, "0.0."+strconv.Itoa(i), time.Now().Add(time.Duration(i)*time.Minute))
	}

	if err := CleanupManaged(context.Background(), launcher); err != nil {
		t.Fatalf("CleanupManaged: %v", err)
	}
	got, err := os.ReadFile(sentinel)
	if err != nil || string(got) != "must survive" {
		t.Fatalf("symlink target was touched: data=%q err=%v", got, err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("unmanaged version symlink should be left alone: info=%v err=%v", info, err)
	}
}

func TestCleanupManagedRemovesOnlyStaleStagingEntries(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix launcher layout test")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	current := seedManagedVersion(t, layout.versionsRoot, "1.0.0", time.Now())
	replaceLauncherSymlink(t, launcher, current)
	if err := os.MkdirAll(layout.stagingRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(layout.stagingRoot, "install-stale")
	fresh := filepath.Join(layout.stagingRoot, "install-fresh")
	if err := os.Mkdir(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * stagingMaxAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	if err := CleanupManaged(context.Background(), launcher); err != nil {
		t.Fatalf("CleanupManaged: %v", err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale staging entry remains, err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh staging entry removed: %v", err)
	}
}

func TestCleanupManagedNeverFollowsStagingRootSymlink(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix symlink safety test")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", "metis")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	current := seedManagedVersion(t, layout.versionsRoot, "1.0.0", time.Now())
	replaceLauncherSymlink(t, launcher, current)
	external := t.TempDir()
	sentinel := filepath.Join(external, "old-but-external")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * stagingMaxAge)
	if err := os.Chtimes(sentinel, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, layout.stagingRoot); err != nil {
		t.Fatal(err)
	}

	if err := CleanupManaged(context.Background(), launcher); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep" {
		t.Fatalf("cleanup followed staging-root symlink: got=%q err=%v", got, err)
	}
}

func TestInstallLockNeverStealsFromLiveOwnerBasedOnAge(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix launcher fixture")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", executableName())
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	current := seedManagedVersion(t, layout.versionsRoot, "1.0.0", time.Now())
	replaceLauncherSymlink(t, launcher, current)
	if err := os.MkdirAll(layout.installLockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	owner := installLockOwner{PID: os.Getpid(), Nonce: "live-owner", CreatedAt: time.Now().Add(-24 * time.Hour).Unix()}
	b, _ := json.Marshal(owner)
	if err := os.WriteFile(filepath.Join(layout.installLockDir, lockOwnerFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(layout.installLockDir, old, old); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	err := CleanupManaged(ctx, launcher)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CleanupManaged error = %v, want deadline while live owner holds old lock", err)
	}
	got, err := readInstallLockOwner(layout.installLockDir)
	if err != nil || got.Nonce != owner.Nonce {
		t.Fatalf("live lock was stolen: owner=%+v err=%v", got, err)
	}
}

func TestInstallLockReclaimsDeadOwner(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix launcher fixture")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", executableName())
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	current := seedManagedVersion(t, layout.versionsRoot, "1.0.0", time.Now())
	replaceLauncherSymlink(t, launcher, current)
	if err := os.MkdirAll(layout.installLockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	owner := installLockOwner{PID: deadPIDForTest(), Nonce: "dead-owner", CreatedAt: time.Now().Add(-time.Hour).Unix()}
	b, _ := json.Marshal(owner)
	if err := os.WriteFile(filepath.Join(layout.installLockDir, lockOwnerFile), b, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CleanupManaged(context.Background(), launcher); err != nil {
		t.Fatalf("CleanupManaged should reclaim dead lock: %v", err)
	}
	if _, err := os.Lstat(layout.installLockDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock dir remains after release, err=%v", err)
	}
}

func TestInstallLockNeverStealsMalformedOwnerBasedOnAge(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix launcher fixture")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", executableName())
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	current := seedManagedVersion(t, layout.versionsRoot, "1.0.0", time.Now())
	replaceLauncherSymlink(t, launcher, current)
	if err := os.MkdirAll(layout.installLockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.installLockDir, lockOwnerFile), []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(layout.installLockDir, old, old); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if err := CleanupManaged(ctx, launcher); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CleanupManaged error = %v, want deadline for ambiguous owner", err)
	}
	if got, err := os.ReadFile(filepath.Join(layout.installLockDir, lockOwnerFile)); err != nil || string(got) != "malformed" {
		t.Fatalf("malformed lock was stolen: got=%q err=%v", got, err)
	}
}

func TestInstallLockNeverReclaimsOwnerlessDirectoryBeingInitialized(t *testing.T) {
	launcher := filepath.Join(t.TempDir(), "bin", executableName())
	layout := layoutForLauncher(launcher)
	ready := make(chan struct{})
	resume := make(chan struct{})
	resumed := false
	defer func() {
		if !resumed {
			close(resume)
		}
	}()
	var paused atomic.Bool
	installLockPendingReadyForTest = func(path string) {
		if paused.CompareAndSwap(false, true) {
			close(ready)
			<-resume
		}
	}
	t.Cleanup(func() { installLockPendingReadyForTest = nil })

	type result struct {
		release func()
		err     error
	}
	aCtx, cancelA := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelA()
	aResult := make(chan result, 1)
	go func() {
		release, err := acquireInstallLock(aCtx, layout)
		aResult <- result{release: release, err: err}
	}()
	<-ready

	// Model a shell installer paused after mkdir but before owner creation.
	// A complete Go pending lock must never replace this ambiguous fixed path,
	// regardless of how old the directory appears.
	if err := os.Mkdir(layout.installLockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(layout.installLockDir, old, old); err != nil {
		t.Fatal(err)
	}

	bctx, cancelB := context.WithTimeout(context.Background(), 150*time.Millisecond)
	bRelease, bErr := acquireInstallLock(bctx, layout)
	cancelB()
	close(resume)
	resumed = true
	a := <-aResult
	if bRelease != nil {
		defer bRelease()
	}
	if a.release != nil {
		defer a.release()
	}

	if !errors.Is(bErr, context.DeadlineExceeded) {
		t.Fatalf("contender acquired/replaced initializing lock: err=%v", bErr)
	}
	if !errors.Is(a.err, context.DeadlineExceeded) {
		t.Fatalf("pending claimant replaced ownerless fixed lock: err=%v", a.err)
	}
	if _, err := os.Lstat(filepath.Join(layout.installLockDir, lockOwnerFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ownerless fixed lock was modified: %v", err)
	}
	assertNoInstallLockPendingArtifacts(t, layout)
}

func TestInstallLockPendingClaimCannotReplaceAnotherWinner(t *testing.T) {
	layout := layoutForLauncher(filepath.Join(t.TempDir(), "bin", executableName()))
	ready := make(chan struct{})
	resume := make(chan struct{})
	resumed := false
	defer func() {
		if !resumed {
			close(resume)
		}
	}()
	var paused atomic.Bool
	installLockPendingReadyForTest = func(path string) {
		if paused.CompareAndSwap(false, true) {
			close(ready)
			<-resume
		}
	}
	t.Cleanup(func() { installLockPendingReadyForTest = nil })

	type result struct {
		release func()
		err     error
	}
	aCtx, cancelA := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelA()
	aResult := make(chan result, 1)
	go func() {
		release, err := acquireInstallLock(aCtx, layout)
		aResult <- result{release: release, err: err}
	}()
	<-ready

	bRelease, err := acquireInstallLock(context.Background(), layout)
	if err != nil {
		t.Fatal(err)
	}
	bOwner, err := readInstallLockOwner(layout.installLockDir)
	if err != nil {
		bRelease()
		t.Fatal(err)
	}
	close(resume)
	resumed = true
	a := <-aResult
	if a.release != nil {
		a.release()
	}
	if !errors.Is(a.err, context.DeadlineExceeded) {
		bRelease()
		t.Fatalf("losing pending claimant unexpectedly acquired lock: %v", a.err)
	}
	current, err := readInstallLockOwner(layout.installLockDir)
	if err != nil || current.Nonce != bOwner.Nonce {
		bRelease()
		t.Fatalf("winner was replaced: owner=%+v err=%v", current, err)
	}
	bRelease()
	assertNoInstallLockPendingArtifacts(t, layout)
}

func TestInstallLockStaleReclaimRevalidatesAfterSerializing(t *testing.T) {
	layout := layoutForLauncher(filepath.Join(t.TempDir(), "bin", executableName()))
	if err := os.MkdirAll(layout.installLockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldOwner := installLockOwner{PID: deadPIDForTest(), Nonce: "dead-owner", CreatedAt: time.Now().Add(-time.Hour).Unix()}
	writeInstallLockOwnerForTest(t, layout.installLockDir, oldOwner)

	ready := make(chan struct{})
	resume := make(chan struct{})
	resumed := false
	defer func() {
		if !resumed {
			close(resume)
		}
	}()
	var paused atomic.Bool
	installLockBeforeStaleClaimForTest = func() {
		if paused.CompareAndSwap(false, true) {
			close(ready)
			<-resume
		}
	}
	t.Cleanup(func() { installLockBeforeStaleClaimForTest = nil })

	aResult := make(chan error, 1)
	go func() { aResult <- quarantineInstallLock(layout.installLockDir, oldOwner) }()
	<-ready
	if err := quarantineInstallLock(layout.installLockDir, oldOwner); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(layout.installLockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	newOwner := installLockOwner{PID: os.Getpid(), Nonce: "new-live-owner", CreatedAt: time.Now().Unix()}
	writeInstallLockOwnerForTest(t, layout.installLockDir, newOwner)
	close(resume)
	resumed = true
	if err := <-aResult; err == nil {
		t.Fatal("paused stale reclaimer removed a successor lock")
	}
	current, err := readInstallLockOwner(layout.installLockDir)
	if err != nil || current.Nonce != newOwner.Nonce {
		t.Fatalf("successor lock was changed: owner=%+v err=%v", current, err)
	}
}

func TestReclaimGuardRecoversDeadOwner(t *testing.T) {
	layout := layoutForLauncher(filepath.Join(t.TempDir(), "bin", executableName()))
	if err := os.MkdirAll(layout.locksRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	target := installLockOwner{PID: deadPIDForTest(), Nonce: "target-owner", CreatedAt: time.Now().Add(-time.Hour).Unix()}
	guardPath := reclaimGuardPath(layout.installLockDir, target)
	if err := os.Mkdir(guardPath, 0o700); err != nil {
		t.Fatal(err)
	}
	deadGuard := reclaimGuardOwner{
		PID:         deadPIDForTest(),
		Nonce:       "dead-reclaimer",
		TargetPID:   target.PID,
		TargetNonce: target.Nonce,
		CreatedAt:   time.Now().Add(-time.Hour).Unix(),
	}
	writeReclaimGuardOwnerForTest(t, guardPath, deadGuard)

	release, err := acquireReclaimGuard(layout.installLockDir, target)
	if err != nil {
		t.Fatalf("recover dead reclaim guard: %v", err)
	}
	current, err := readReclaimGuardOwner(guardPath)
	if err != nil || current.Nonce == deadGuard.Nonce || current.PID != os.Getpid() {
		release()
		t.Fatalf("new reclaim guard not published: owner=%+v err=%v", current, err)
	}
	retired := guardPath + ".retired." + deadGuard.Nonce
	if _, err := os.Lstat(retired); err != nil {
		release()
		t.Fatalf("dead guard did not leave non-reusable tombstone: %v", err)
	}
	release()
	if _, err := os.Lstat(guardPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released guard remains: %v", err)
	}
	if _, err := os.Lstat(retired); err != nil {
		t.Fatalf("retired tombstone was removed: %v", err)
	}
}

func TestReclaimGuardRecoveryCannotRetireSuccessor(t *testing.T) {
	layout := layoutForLauncher(filepath.Join(t.TempDir(), "bin", executableName()))
	if err := os.MkdirAll(layout.locksRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	target := installLockOwner{PID: deadPIDForTest(), Nonce: "target-owner", CreatedAt: time.Now().Add(-time.Hour).Unix()}
	guardPath := reclaimGuardPath(layout.installLockDir, target)
	if err := os.Mkdir(guardPath, 0o700); err != nil {
		t.Fatal(err)
	}
	deadGuard := reclaimGuardOwner{
		PID:         deadPIDForTest(),
		Nonce:       "dead-reclaimer",
		TargetPID:   target.PID,
		TargetNonce: target.Nonce,
		CreatedAt:   time.Now().Add(-time.Hour).Unix(),
	}
	writeReclaimGuardOwnerForTest(t, guardPath, deadGuard)

	ready := make(chan struct{})
	resume := make(chan struct{})
	resumed := false
	defer func() {
		if !resumed {
			close(resume)
		}
	}()
	var paused atomic.Bool
	reclaimGuardBeforeRetireForTest = func() {
		if paused.CompareAndSwap(false, true) {
			close(ready)
			<-resume
		}
	}
	t.Cleanup(func() { reclaimGuardBeforeRetireForTest = nil })

	type result struct {
		release func()
		err     error
	}
	aResult := make(chan result, 1)
	go func() {
		release, err := acquireReclaimGuard(layout.installLockDir, target)
		aResult <- result{release: release, err: err}
	}()
	<-ready
	bRelease, err := acquireReclaimGuard(layout.installLockDir, target)
	if err != nil {
		t.Fatal(err)
	}
	bOwner, err := readReclaimGuardOwner(guardPath)
	if err != nil {
		bRelease()
		t.Fatal(err)
	}
	close(resume)
	resumed = true
	a := <-aResult
	if a.release != nil {
		a.release()
	}
	if a.err == nil {
		bRelease()
		t.Fatal("paused stale observer acquired or retired the successor guard")
	}
	current, err := readReclaimGuardOwner(guardPath)
	if err != nil || current.Nonce != bOwner.Nonce {
		bRelease()
		t.Fatalf("successor reclaim guard was changed: owner=%+v err=%v", current, err)
	}
	bRelease()
}

func TestCleanupManagedRemovesOnlyOldPendingLockArtifacts(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix launcher fixture")
	}
	base := t.TempDir()
	launcher := filepath.Join(base, "bin", executableName())
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := layoutForLauncher(launcher)
	current := seedManagedVersion(t, layout.versionsRoot, "1.0.0", time.Now())
	replaceLauncherSymlink(t, launcher, current)
	if err := os.MkdirAll(layout.locksRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPending := layout.installLockDir + ".pending.old"
	freshPending := layout.installLockDir + ".pending.fresh"
	for _, path := range []string{oldPending, freshPending} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleLockArtifactAge)
	if err := os.Chtimes(oldPending, old, old); err != nil {
		t.Fatal(err)
	}

	if err := CleanupManaged(context.Background(), launcher); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(oldPending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old pending lock artifact remains: %v", err)
	}
	if info, err := os.Lstat(freshPending); err != nil || !info.IsDir() {
		t.Fatalf("fresh pending lock artifact was removed: info=%v err=%v", info, err)
	}
}

func writeInstallLockOwnerForTest(t *testing.T, lockDir string, owner installLockOwner) {
	t.Helper()
	b, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, lockOwnerFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeReclaimGuardOwnerForTest(t *testing.T, guardDir string, owner reclaimGuardOwner) {
	t.Helper()
	b, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(guardDir, lockOwnerFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertNoInstallLockPendingArtifacts(t *testing.T, layout installLayout) {
	t.Helper()
	entries, err := os.ReadDir(layout.locksRoot)
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Base(layout.installLockDir) + ".pending."
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			t.Errorf("pending lock artifact remains: %s", entry.Name())
		}
	}
}

func deadPIDForTest() int {
	// Kernels reject this value rather than recycling it as a user process ID.
	return int(^uint32(0) >> 1)
}
