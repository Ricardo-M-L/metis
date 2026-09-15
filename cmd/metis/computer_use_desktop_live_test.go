//go:build darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestComputerUseLiveAXDesktop deliberately requires a human/CUA operator to
// submit the printed fixture-only prompt through the native Desktop UI. The
// target fixture must then be foreground. It does not automate OS permissions,
// other applications, or the user's installed METIS. Success additionally needs
// real session tool evidence, not merely a UI/model completion message.
func TestComputerUseLiveAXDesktop(t *testing.T) {
	if os.Getenv("METIS_CU_DESKTOP_LIVE_TEST") != "1" {
		t.Skip("isolated native Desktop + real API acceptance is opt-in")
	}
	cli, helper, desktop := os.Getenv("METIS_CU_TEST_CLI"), os.Getenv("METIS_CU_TEST_HELPER"), os.Getenv("METIS_CU_TEST_DESKTOP")
	if !filepath.IsAbs(cli) || !filepath.IsAbs(helper) || !filepath.IsAbs(desktop) {
		t.Fatal("explicit absolute test CLI/helper/Desktop binaries required")
	}
	deadline := time.Now().Add(300 * time.Second)
	env := newCULiveAXEnvironment(t, deadline)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	runCULiveAXCommand(t, ctx, env, cli, "cu", "install", "--from", helper, "--json")
	runCULiveAXCommand(t, ctx, env, cli, "cu", "enable", "--json")
	cmd := exec.Command(desktop, "--workspace", env.Home, "--metis-bin", cli)
	cmd.Dir, cmd.Env = env.Home, append(append([]string{}, env.Env...), "METIS_BIN="+cli)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var suppressed bytes.Buffer
	cmd.Stdout, cmd.Stderr = &suppressed, &suppressed
	if err := cmd.Start(); err != nil {
		t.Fatalf("launch isolated native Desktop: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	closed := false
	t.Cleanup(func() {
		if closed {
			return
		}
		// This is our freshly created process group, never a user process or
		// broad executable-name match. Keep credential cleanup after reaping.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	})
	t.Logf("DESKTOP_READY pid=%d; submit this fixture-only prompt through the native input, then foreground the authorized fixture: %s", cmd.Process.Pid, env.Prompt)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	verified := false
	var lastEvidence cuLiveAXEvidence
	var lastFiles cuLiveAXSessionFiles
	var lastErr error
	var verifiedSession []byte
	for !verified {
		select {
		case <-ctx.Done():
			t.Logf("Desktop evidence counts=%+v files=%+v last-check=%v", lastEvidence, lastFiles, lastErr)
			t.Fatal("Desktop session did not demonstrate the full AX task before the acceptance deadline; output suppressed")
		case err := <-done:
			closed = true
			t.Fatalf("Desktop exited before verified session evidence: %v", err)
		case <-tick.C:
			data, files, err := findCULiveAXPromptSession(env.MetisHome, env.Prompt)
			lastFiles, lastErr = files, err
			if err != nil {
				continue
			}
			// Sessions are still being appended: incomplete evidence is
			// not failure yet and never causes a mutation to be retried.
			lastEvidence, lastErr = inspectCULiveAXSession(data, env.FixturePID, env.Marker)
			if lastErr == nil {
				verified = true
				verifiedSession = data
			}
		}
	}
	t.Logf("DESKTOP_AX_VERIFIED: provisional paired evidence=%+v; inspect final UI then close this test Desktop normally for final session verification", lastEvidence)
	select {
	case err := <-done:
		closed = true
		if err != nil {
			t.Fatalf("test Desktop did not exit cleanly: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("verified Desktop was not closed before deadline; forcing isolated process cleanup")
	}
	lastEvidence, lastFiles, lastErr = inspectCULiveAXDesktopFinalSession(env, verifiedSession)
	if lastErr != nil {
		t.Logf("Desktop final evidence counts=%+v files=%+v", lastEvidence, lastFiles)
		t.Fatal(lastErr)
	}
	t.Log("native Desktop + real model AX task passed with isolated fixture session and normal shutdown")
}

func inspectCULiveAXDesktopFinalSession(env cuLiveAXEnvironment, verifiedSession []byte) (cuLiveAXEvidence, cuLiveAXSessionFiles, error) {
	// A passing prefix only permits the operator to close the Desktop. Re-read
	// the unique prompt conversation after exit so later calls, errors, and
	// duplicate conversations cannot evade the acceptance inspector.
	data, files, err := findCULiveAXPromptSession(env.MetisHome, env.Prompt)
	if err != nil {
		return cuLiveAXEvidence{}, files, err
	}
	if len(verifiedSession) == 0 || !bytes.HasPrefix(data, verifiedSession) {
		return cuLiveAXEvidence{}, files, errors.New("final Desktop session does not preserve the previously verified conversation")
	}
	evidence, err := inspectCULiveAXSession(data, env.FixturePID, env.Marker)
	return evidence, files, err
}
