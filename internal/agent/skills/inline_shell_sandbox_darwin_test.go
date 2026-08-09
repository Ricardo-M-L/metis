//go:build darwin

package skills

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

func TestInlineSkillShellUsesRuntimeSandbox(t *testing.T) {
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
	body := "!`printf yes > " + strconv.Quote(inside) + "`\n!`printf nope > " + strconv.Quote(outside) + "`"
	got := ExpandInlineShellWithSandbox(context.Background(), body, cwd, manager)
	if !strings.Contains(got, "shell error") {
		t.Fatalf("outside denial was not surfaced: %s", got)
	}
	if data, err := os.ReadFile(inside); err != nil || string(data) != "yes" {
		t.Fatalf("cwd write = %q, %v", data, err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside canary exists or stat failed: %v", err)
	}
}
