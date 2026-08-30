package builtin

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestConfigureWebBrowseProcessInstallsBoundedCancellation(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "metis-webbrowse-test-command")
	configureWebBrowseProcess(cmd)
	if cmd.Cancel == nil {
		t.Fatal("WebBrowse command has no cancellation hook")
	}
	if cmd.WaitDelay != webBrowseProcessWaitDelay {
		t.Fatalf("WebBrowse WaitDelay = %s, want %s", cmd.WaitDelay, webBrowseProcessWaitDelay)
	}
	if cmd.WaitDelay <= 0 || cmd.WaitDelay > 5*time.Second {
		t.Fatalf("WebBrowse WaitDelay is not a short hard bound: %s", cmd.WaitDelay)
	}
}
