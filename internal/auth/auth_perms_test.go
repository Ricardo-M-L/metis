package auth

// auth_perms_test.go pins the 0600 self-healing behavior added on
// 2026-05-08 (#23 in /Users/ricardo/Documents/公司学习文件/我自己的agent的cli/测试报告/00-SUMMARY.md).
// The threat: if a user accidentally `chmod 644 ~/.metis/auth.json`
// — or restores from a backup with default perms — every shell user
// on the box can read their API keys. Load() now self-heals back to
// 0600 and writes a warning to stderr.

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestLoad_TightensLoosePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perms model differs on Windows")
	}
	dir := withTempHome(t)
	path := dir + "/auth.json"

	// Plant a credentials file with world-readable bits.
	if err := os.WriteFile(path, []byte(`{"openai":{"type":"api","key":"sk-test"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Capture stderr so we can confirm the warning fires.
	r, w, _ := os.Pipe()
	saved := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = saved }()

	if _, err := Load(); err != nil {
		_ = w.Close()
		t.Fatalf("Load: %v", err)
	}
	_ = w.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	stderr := string(buf[:n])

	if !strings.Contains(stderr, "loose perms") {
		t.Errorf("expected stderr warning about loose perms; got %q", stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy auth.json was not removed after migration: %v", err)
	}
	st, err := os.Stat(Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("migrated auth.json should be 0600; got %#o", got)
	}
}

// TestLoad_AcceptsCorrectPerms — the warning must be silent when the
// file is already tight. Otherwise every successful boot would print
// a useless line.
func TestLoad_AcceptsCorrectPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perms model differs on Windows")
	}
	dir := withTempHome(t)
	path := dir + "/auth.json"

	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	r, w, _ := os.Pipe()
	saved := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = saved }()

	if _, err := Load(); err != nil {
		_ = w.Close()
		t.Fatalf("Load: %v", err)
	}
	_ = w.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	if n > 0 {
		t.Errorf("expected silent Load on 0600 file; got stderr %q", string(buf[:n]))
	}
}

// TestLoad_MissingFileNoWarning — the absence of auth.json (fresh
// install) must not print anything.
func TestLoad_MissingFileNoWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perms model differs on Windows")
	}
	withTempHome(t)

	r, w, _ := os.Pipe()
	saved := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = saved }()

	if _, err := Load(); err != nil {
		_ = w.Close()
		t.Fatalf("Load: %v", err)
	}
	_ = w.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	if n > 0 {
		t.Errorf("expected silent Load when file missing; got stderr %q", string(buf[:n]))
	}
}
