package computeruse

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestInstallLockSubprocess is launched by tests below. os.Exit and forced
// termination intentionally skip deferred unlocks to test OS crash recovery.
func TestInstallLockSubprocess(t *testing.T) {
	root := os.Getenv("METIS_CU_TEST_LOCK_ROOT")
	if root == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, _, err := New(root).lock(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(23)
	}
	defer unlock()
	fmt.Fprintln(os.Stdout, "locked")
	if os.Getenv("METIS_CU_TEST_LOCK_MODE") == "exit" {
		os.Exit(42)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(43)
}

func installLockChild(t *testing.T, root, mode string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, executable, "-test.run=^TestInstallLockSubprocess$")
	command.Env = append(os.Environ(), "METIS_CU_TEST_LOCK_ROOT="+root, "METIS_CU_TEST_LOCK_MODE="+mode)
	command.WaitDelay = time.Second
	return command
}

func assertInstallAfterChildExit(t *testing.T, root string) {
	t.Helper()
	m := New(root)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if runtime.GOOS == "windows" {
		// The executable fixture uses /bin/sh. Still prove process-lock recovery
		// on Windows without pretending that a Unix fixture can execute there.
		unlock, _, err := m.lock(ctx)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
		return
	}
	status, err := m.InstallLocal(ctx, fixtureExecutable(t, fixtureDescription("after-crash")))
	if err != nil || !status.Installed {
		t.Fatalf("install after crashed holder: %+v, %v", status, err)
	}
}

func TestInstallLockReleasedOnProcessExit(t *testing.T) {
	root := t.TempDir()
	command := installLockChild(t, root, "exit")
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 42 || strings.TrimSpace(string(output)) != "locked" {
		t.Fatalf("child failed before acquiring lock: output=%q error=%v", output, err)
	}
	assertInstallAfterChildExit(t, root)
}

func TestInstallLockReleasedOnForcedTermination(t *testing.T) {
	root := t.TempDir()
	command := installLockChild(t, root, "hold")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("child did not report acquired lock: line=%q err=%v", line, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if unlock, _, err := New(root).lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("another process acquired the held lock: %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	waited = true
	if err == nil {
		t.Fatal("expected child termination to bypass normal release")
	}
	assertInstallAfterChildExit(t, root)
}

func TestInstallLockPersistentPrivateFile(t *testing.T) {
	m := New(t.TempDir())
	firstUnlock, directory, err := m.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer firstUnlock()
	filename := filepath.Join(directory, ".install-lock")
	firstInfo, err := os.Lstat(filename)
	if err != nil || !firstInfo.Mode().IsRegular() {
		t.Fatalf("lock is not a regular file: %v", err)
	}
	if runtime.GOOS != "windows" && firstInfo.Mode().Perm() != 0600 {
		t.Fatalf("lock permissions=%o", firstInfo.Mode().Perm())
	}
	firstUnlock()
	secondUnlock, _, err := m.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer secondUnlock()
	secondInfo, err := os.Lstat(filename)
	if err != nil || !os.SameFile(firstInfo, secondInfo) {
		t.Fatalf("lock inode replaced between owners: %v", err)
	}
	// A repeated release must not close a descriptor now reused by a waiter.
	firstUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if unlock, _, err := m.lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("repeated release affected the next owner: %v", err)
	}
}

func TestInstallLockRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	m := New(t.TempDir())
	directory, _ := m.directory()
	if err := checkManagedDirectories(directory, true); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "unrelated")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(directory, ".install-lock")
	if err := os.Symlink(target, filename); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if unlock, _, err := m.lock(context.Background()); err == nil {
		unlock()
		t.Fatal("accepted a symlink lock")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("symlink target changed: %q %v", data, err)
	}
	info, err := os.Lstat(filename)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was removed or replaced: %v", err)
	}
}

func TestInstallLockPreservesExistingDirectory(t *testing.T) {
	m := New(t.TempDir())
	directory, _ := m.directory()
	if err := checkManagedDirectories(directory, true); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(directory, ".install-lock")
	if err := os.Mkdir(filename, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(filename, "owner-state")
	if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if unlock, _, err := m.lock(context.Background()); err == nil {
		unlock()
		t.Fatal("accepted or migrated a directory lock")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("existing directory lock was changed: %q %v", data, err)
	}
}
