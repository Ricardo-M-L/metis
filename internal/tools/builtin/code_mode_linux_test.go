//go:build linux

package builtin

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

func TestRunCodeLinuxSandboxManagerOwnedTempE2E(t *testing.T) {
	diagnostic := sandbox.Doctor()
	if !diagnostic.Available {
		t.Skipf("bubblewrap unavailable: %v", diagnostic.Err)
	}
	probe := exec.Command(diagnostic.Executable,
		"--die-with-parent", "--ro-bind", "/", "/", "--", "/bin/true")
	if err := probe.Run(); err != nil {
		t.Skipf("bubblewrap cannot create an unprivileged sandbox here: %v", err)
	}

	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{
		Mode:     string(sandbox.ModePermissions),
		TempRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	runner := NewRunCodeWithSandbox(nil, manager)
	result, err := runner.Execute(context.Background(), map[string]any{
		"language": "bash",
		"code": `probe="$TMPDIR/metis-runcode-probe"
printf temp-ok > "$probe"
test "$(cat "$probe")" = temp-ok
rm "$probe"
printf sandbox-ok`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.TrimSpace(result.Output) != "sandbox-ok" {
		t.Fatalf("RunCode sandbox result = %#v", result)
	}
}
