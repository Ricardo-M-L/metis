//go:build !windows

package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestScopeGitCommandOwnsAndCancelsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	fakeGit := filepath.Join(dir, "git")
	script := fmt.Sprintf("#!/bin/sh\nsleep 30 &\nchild=$!\nprintf '%%s' \"$child\" > %q\nwait\n", pidFile)
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := newScopeGitCommand(ctx, dir, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		cancel()
		t.Fatalf("scope git lacks a dedicated process group: %+v", cmd.SysProcAttr)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			childPID, readErr = strconv.Atoi(strings.TrimSpace(string(raw)))
			if readErr == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 {
		cancel()
		select {
		case <-done:
		case <-time.After(scopeProcessWaitLimit + time.Second):
		}
		t.Fatal("fake git child pid was not published")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(scopeProcessWaitLimit + time.Second):
		t.Fatal("scope git cancellation did not return within the bounded wait")
	}

	for deadline := time.Now().Add(2 * time.Second); ; {
		probeErr := syscall.Kill(childPID, 0)
		if errors.Is(probeErr, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scope git descendant pid %d is still alive after cancellation (probe: %v)", childPID, probeErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestScopeGitCommandDoesNotInheritProviderSecrets(t *testing.T) {
	dir := t.TempDir()
	captured := filepath.Join(dir, "captured-env")
	fakeGit := filepath.Join(dir, "git")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"${OPENAI_API_KEY-unset}\" > %q\nexit 0\n", captured)
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENAI_API_KEY", "scope-provider-secret")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd, err := newScopeGitCommand(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("fake scope git failed: %v", err)
	}
	got, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("fake git did not run: %v", err)
	}
	if string(got) != "unset" {
		t.Fatalf("scope git inherited provider secret: %q", got)
	}
}
