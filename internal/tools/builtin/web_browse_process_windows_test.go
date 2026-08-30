//go:build windows

package builtin

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestKillWebBrowseProcessTreePlatformBoundsTaskkill(t *testing.T) {
	previous := webBrowseTaskkillCommandContext
	t.Cleanup(func() { webBrowseTaskkillCommandContext = previous })
	webBrowseTaskkillCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWebBrowseTaskkillHelperProcess$")
	}

	start := time.Now()
	killWebBrowseProcessTreePlatform(&os.Process{Pid: 123}, 40*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Fatalf("WebBrowse taskkill returned before its deadline after %s", elapsed)
	} else if elapsed > time.Second {
		t.Fatalf("WebBrowse taskkill exceeded its hard bound: %s", elapsed)
	}
}

func TestWebBrowseTaskkillHelperProcess(t *testing.T) {
	time.Sleep(30 * time.Second)
}
