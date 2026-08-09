//go:build darwin

package slash

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

func TestCustomCommandShellUsesRuntimeSandbox(t *testing.T) {
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

	cwd := t.TempDir()
	external := t.TempDir()
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{Mode: "permissions", TempRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	inside := filepath.Join(cwd, "inside.txt")
	outside := filepath.Join(external, "outside.txt")
	template := "inside !`printf yes > " + strconv.Quote(inside) + "` outside !`printf nope > " + strconv.Quote(outside) + "`"
	got := renderTemplateWithSandbox(template, "", true, cwd, manager)
	if !strings.Contains(got, "exited") {
		t.Fatalf("outside denial was not surfaced: %s", got)
	}
	if data, err := os.ReadFile(inside); err != nil || string(data) != "yes" {
		t.Fatalf("cwd write = %q, %v", data, err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside canary exists or stat failed: %v", err)
	}
}
