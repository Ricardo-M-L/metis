package config

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const userConfigLockHelperEnv = "METIS_TEST_HOLD_USER_CONFIG_LOCK"

// TestUserConfigLockHelperProcess is entered only by the subprocess started by
// TestUserConfigWritesSerializeAcrossProcesses. Keeping the holder in another
// process verifies the OS lock rather than the package-level mutex.
func TestUserConfigLockHelperProcess(t *testing.T) {
	if os.Getenv(userConfigLockHelperEnv) != "1" {
		return
	}
	ready := os.Getenv("METIS_TEST_CONFIG_LOCK_READY")
	release := os.Getenv("METIS_TEST_CONFIG_LOCK_RELEASE")
	if ready == "" || release == "" {
		t.Fatal("config-lock helper paths are missing")
	}
	err := withUserConfigWriteLockTimeout(5*time.Second, func(string) error {
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			return err
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(release); err == nil {
				return nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if time.Now().After(deadline) {
				return errors.New("timed out waiting for config-lock release signal")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUserConfigWritesSerializeAcrossProcesses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	path := filepath.Join(home, "config.toml")
	original := []byte("# preserve me\n[provider]\ndefault = \"anthropic\"\n\n[provider.openai]\napi_key = \"secret-must-stay-untouched\"\n\n[future]\nflag = \"unknown-must-stay\"\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	ready := filepath.Join(home, "holder.ready")
	release := filepath.Join(home, "holder.release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestUserConfigLockHelperProcess$")
	cmd.Env = append(os.Environ(),
		userConfigLockHelperEnv+"=1",
		"METIS_TEST_CONFIG_LOCK_READY="+ready,
		"METIS_TEST_CONFIG_LOCK_RELEASE="+release,
	)
	var helperOutput bytes.Buffer
	cmd.Stdout = &helperOutput
	cmd.Stderr = &helperOutput
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	helperDone := false
	defer func() {
		if helperDone {
			return
		}
		_ = os.WriteFile(release, []byte("release"), 0o600)
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	waitForConfigLockTestFile(t, ready, 3*time.Second)

	entered := false
	started := time.Now()
	err := withUserConfigWriteLockTimeout(80*time.Millisecond, func(string) error {
		entered = true
		return nil
	})
	if !errors.Is(err, errUserConfigLockTimeout) {
		t.Fatalf("contended lock error = %v, want %v", err, errUserConfigLockTimeout)
	}
	if entered {
		t.Fatal("contended transaction ran without owning the lock")
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond || elapsed > time.Second {
		t.Fatalf("bounded lock wait took %s", elapsed)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unchanged, original) {
		t.Fatal("timed-out lock attempt changed config.toml")
	}

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- SaveUserProviderDefault("openai")
	}()
	select {
	case err := <-writeDone:
		t.Fatalf("public config writer did not wait for cross-process lock: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	unchanged, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unchanged, original) {
		t.Fatal("waiting public config writer changed config.toml before acquiring the lock")
	}

	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write after lock release: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("public config writer did not continue after lock release")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("config-lock helper failed: %v\n%s", err, helperOutput.String())
	}
	helperDone = true

	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# preserve me", `default = "openai"`, `api_key = "secret-must-stay-untouched"`, `flag = "unknown-must-stay"`} {
		if !strings.Contains(string(updated), want) {
			t.Errorf("updated config lost %q:\n%s", want, updated)
		}
	}

	lockInfo, err := os.Stat(filepath.Join(home, userConfigLockFilename))
	if err != nil {
		t.Fatalf("stat user config lock: %v", err)
	}
	if got := lockInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("user config lock mode = %#o, want 0600", got)
	}
}

func waitForConfigLockTestFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", filepath.Base(path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
