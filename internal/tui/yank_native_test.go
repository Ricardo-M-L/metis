package tui

// yank_native_test.go — locks the OS-clipboard write path added
// 2026-05-11 (image #3 user report: macOS Terminal.app eats OSC 52
// silently, so Cmd+C in metis chat never reaches the OS clipboard).
//
// We test runClipWrite — the lower-level shell-out helper — rather
// than writeClipboardNative directly, because the latter is OS-
// dispatched and would skip code paths on the test runner's host.

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestRunClipWrite_MissingToolReturnsFalse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if got := runClipWrite(ctx, "anything", "definitely-not-a-real-binary-1234567"); got {
		t.Errorf("expected false for nonexistent tool, got true")
	}
}

func TestRunClipWrite_PbcopyRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("pbcopy is macOS-only")
	}
	if _, err := exec.LookPath("pbcopy"); err != nil {
		t.Skip("pbcopy not in PATH")
	}
	if _, err := exec.LookPath("pbpaste"); err != nil {
		t.Skip("pbpaste not in PATH")
	}
	// Save the user's existing clipboard so we restore it after.
	orig, _ := exec.Command("pbpaste").Output()
	defer func() {
		// Best-effort restore.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		runClipWrite(ctx, string(orig), "pbcopy")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	payload := "metis-yank-native-test-✓-中文混合"
	if !runClipWrite(ctx, payload, "pbcopy") {
		t.Fatalf("runClipWrite(pbcopy) returned false on macOS — wrong return path")
	}
	got, err := exec.Command("pbpaste").Output()
	if err != nil {
		t.Fatalf("pbpaste readback: %v", err)
	}
	if string(got) != payload {
		t.Errorf("pbcopy roundtrip lost data:\n got %q\nwant %q", string(got), payload)
	}
}

func TestRunClipWrite_RespectsContextCancellation(t *testing.T) {
	// Cancelled context → tool gets killed before it can succeed.
	// We use `cat` (always present, reads stdin then waits for EOF)
	// to model a slow-write tool, then immediately cancel.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := runClipWrite(ctx, "anything", "cat"); got {
		t.Errorf("cancelled context should yield false, got true")
	}
}
