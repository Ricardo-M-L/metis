//go:build aix || android || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenCredentialStoreFileRejectsFIFOWithoutWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		file, _, err := openCredentialStoreFile(path, authStoreMaxJSONBytes, false)
		if file != nil {
			_ = file.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO rejection = %v", err)
		}
	case <-time.After(time.Second):
		// Release a blocking reader before failing, so the regression cannot
		// strand a goroutine or hold a store lock in later tests.
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			t.Fatalf("open FIFO cleanup writer: %v", err)
		}
		defer unix.Close(fd)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("FIFO reader did not stop after attaching cleanup writer")
		}
		t.Fatal("credential-store open blocked on a FIFO without a writer")
	}
}

func TestOpenCredentialStoreFileRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	file, found, err := openCredentialStoreFile(link, authStoreMaxJSONBytes, true)
	if file != nil {
		_ = file.Close()
	}
	if err == nil || found || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("open symlink = (found=%v, err=%v), want symlink rejection", found, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("symlink target permissions changed to %#o", got)
	}
}

func TestValidateOpenedCredentialStoreRejectsReplacementAndSecuresOnlyOpenedInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	held := filepath.Join(dir, "opened.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(path, held); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"attacker":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	err = validateOpenedCredentialStore(path, file, authStoreMaxJSONBytes, false)
	if err == nil || !strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("replacement accepted: %v", err)
	}
	openedInfo, err := os.Stat(held)
	if err != nil {
		t.Fatal(err)
	}
	if got := openedInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("opened inode permissions = %#o, want 0600", got)
	}
	replacementInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := replacementInfo.Mode().Perm(); got != 0o644 {
		t.Fatalf("replacement inode permissions changed to %#o", got)
	}
}
