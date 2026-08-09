//go:build darwin

package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestLocalBangCommandUsesRuntimeSandbox(t *testing.T) {
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.Doctor().Err)
	}
	probe := exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1)\n(allow default)\n", "/usr/bin/true")
	if output, err := probe.CombinedOutput(); err != nil {
		if strings.Contains(string(output), "Operation not permitted") {
			t.Skipf("host forbids nested Seatbelt: %s", output)
		}
		t.Fatalf("sandbox preflight: %v: %s", err, output)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	insideFile, err := os.CreateTemp(cwd, ".metis-sandbox-tui-")
	if err != nil {
		t.Fatal(err)
	}
	inside := insideFile.Name()
	if err := insideFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(inside); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(inside) })

	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: "permissions", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	cfg := &config.Config{}
	cfg.Tools.Bash.Shell = "/bin/sh"
	gate := permission.New(permission.ModeBypassPermissions)
	// Intentionally omit Bash from the model-facing registry. Local !cmd must
	// use the runtime-owned manager directly even when Bash is disabled/hidden.
	reg := tools.NewRegistry()
	loop := agent.NewLoop(nil, reg, gate, nil, "test", 1)
	m := &Model{cfg: cfg, loop: loop, ext: ExternalHooks{Sandbox: manager}}

	outside := filepath.Join(external, "outside.txt")
	msg := m.bashLocalCmd("printf yes > " + strconv.Quote(inside))().(bashLocalResultMsg)
	if msg.row.Role != "bash" {
		t.Fatalf("cwd command failed: %+v", msg.row)
	}
	msg = m.bashLocalCmd("printf nope > " + strconv.Quote(outside))().(bashLocalResultMsg)
	if msg.row.Role != "bash-error" || !strings.Contains(msg.row.Content, "exit") {
		t.Fatalf("outside denial not surfaced: %+v", msg.row)
	}
	if data, err := os.ReadFile(inside); err != nil || string(data) != "yes" {
		t.Fatalf("cwd write = %q, %v", data, err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside canary exists or stat failed: %v", err)
	}
}
