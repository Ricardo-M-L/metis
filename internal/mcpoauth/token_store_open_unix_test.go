//go:build aix || android || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package mcpoauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTokenStoreRejectsFIFOWithoutHoldingGlobalLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp-oauth.json")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	store := &TokenStore{path: path}
	done := make(chan error, 1)
	go func() {
		_, err := store.GetEntry("test-server")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO rejection = %v", err)
		}
	case <-time.After(time.Second):
		// A nonblocking writer safely releases the unfixed reader and its
		// global mutex, even when this test intentionally fails.
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
		t.Fatal("token-store read blocked on a FIFO while holding the global lock")
	}
	// A separate store must remain usable after rejecting the FIFO.
	other := &TokenStore{path: filepath.Join(dir, "other.json")}
	if _, err := other.GetEntry("test-server"); err != nil {
		t.Fatalf("read after FIFO rejection: %v", err)
	}
}

func TestOpenTokenStoreFileRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "mcp-oauth.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	file, found, err := openTokenStoreFile(link, tokenStoreMaxJSONBytes)
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

func TestValidateOpenedTokenStoreRejectsReplacementAndSecuresOnlyOpenedInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp-oauth.json")
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

	err = validateOpenedTokenStore(path, file, tokenStoreMaxJSONBytes)
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
