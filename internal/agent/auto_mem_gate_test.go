package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// fakeReadOnlyTool implements pubtool.ReadOnlyAware so the gate can
// route to the read-only-aware path without spinning up the real Bash.
type fakeBashTool struct {
	forkFakeTool
	readOnly bool
}

func (t fakeBashTool) IsReadOnly(map[string]any) bool { return t.readOnly }

func TestAutoMemGate_AllowsReadGrepGlob(t *testing.T) {
	gate := CreateAutoMemCanUseTool(t.TempDir(), tools.NewRegistry())
	for _, name := range []string{"Read", "Grep", "Glob"} {
		if ok, _ := gate(context.Background(), name, nil); !ok {
			t.Errorf("%s should be allowed", name)
		}
	}
}

func TestAutoMemGate_DeniesCredentialReadsAcrossReadAndGrep(t *testing.T) {
	root := t.TempDir()
	t.Setenv("METIS_HOME", root)
	gate := CreateAutoMemCanUseTool(t.TempDir(), tools.NewRegistry())
	secret := filepath.Join(root, "auth.json")

	for _, tc := range []struct {
		name  string
		input map[string]any
	}{
		{name: "Read", input: map[string]any{"path": secret}},
		{name: "Grep", input: map[string]any{"root": root, "pattern": "token"}},
	} {
		ok, reason := gate(context.Background(), tc.name, tc.input)
		if ok || !strings.Contains(reason, "credential") {
			t.Errorf("%s credential read = (%v, %q), want credential-boundary deny", tc.name, ok, reason)
		}
	}
}

func TestAutoMemGate_GrepWithoutRootUsesCredentialCWD(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_HOME", root)

	gate := CreateAutoMemCanUseTool(t.TempDir(), tools.NewRegistry())
	ok, reason := gate(context.Background(), "Grep", map[string]any{"pattern": "token"})
	if ok || !strings.Contains(reason, "credential") {
		t.Fatalf("no-root Grep in METIS_HOME = (%v, %q), want credential-boundary deny", ok, reason)
	}
}

func TestAutoMemGate_ReadResolvesCredentialSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	root := t.TempDir()
	t.Setenv("METIS_HOME", root)
	secret := filepath.Join(root, "mcp-oauth.json")
	if err := os.WriteFile(secret, []byte(`{"token":"opaque"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "notes.json")
	if err := os.Symlink(secret, alias); err != nil {
		t.Fatal(err)
	}
	gate := CreateAutoMemCanUseTool(t.TempDir(), tools.NewRegistry())
	if ok, reason := gate(context.Background(), "Read", map[string]any{"path": alias}); ok || !strings.Contains(reason, "credential") {
		t.Fatalf("credential symlink read = (%v, %q), want deny", ok, reason)
	}
}

func TestAutoMemGate_BashReadOnlyStillDenied(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(fakeBashTool{forkFakeTool: forkFakeTool{name: "Bash"}, readOnly: true})
	gate := CreateAutoMemCanUseTool(t.TempDir(), reg)
	ok, reason := gate(context.Background(), "Bash", map[string]any{"command": "ls"})
	if ok || !strings.Contains(reason, "shell tools are disabled") {
		t.Fatalf("read-only Bash = (%v, %q), want fail-closed shell denial", ok, reason)
	}
}

func TestAutoMemGate_BashWriteDenied(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(fakeBashTool{forkFakeTool: forkFakeTool{name: "Bash"}, readOnly: false})
	gate := CreateAutoMemCanUseTool(t.TempDir(), reg)
	ok, reason := gate(context.Background(), "Bash", map[string]any{"command": "rm /etc"})
	if ok {
		t.Fatalf("write Bash should be denied")
	}
	if !strings.Contains(reason, "shell tools are disabled") {
		t.Errorf("reason missing shell denial: %q", reason)
	}
}

func TestAutoMemGate_BashUnregisteredDenied(t *testing.T) {
	gate := CreateAutoMemCanUseTool(t.TempDir(), tools.NewRegistry())
	ok, reason := gate(context.Background(), "Bash", nil)
	if ok {
		t.Fatalf("unregistered Bash should fail closed")
	}
	if !strings.Contains(reason, "shell tools are disabled") {
		t.Errorf("reason should mention shell denial: %q", reason)
	}
}

func TestAutoMemGate_NilRegistryFailsClosed(t *testing.T) {
	gate := CreateAutoMemCanUseTool(t.TempDir(), nil)
	ok, reason := gate(context.Background(), "Bash", map[string]any{"command": "ls"})
	if ok || !strings.Contains(reason, "shell tools are disabled") {
		t.Fatalf("nil registry Bash = (%v, %q), want fail-closed", ok, reason)
	}
}

func TestAutoMemGate_DeniesMCPResourcesByDefault(t *testing.T) {
	gate := CreateAutoMemCanUseTool(t.TempDir(), tools.NewRegistry())
	ok, reason := gate(context.Background(), "ReadMcpResource", map[string]any{
		"server": "private-vault",
		"uri":    "secret://provider/token",
	})
	if ok || !strings.Contains(reason, "not memory-safe") {
		t.Fatalf("ReadMcpResource = (%v, %q), want default deny", ok, reason)
	}
}

func TestAutoMemGate_BashNoReadOnlyImpl(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(forkFakeTool{name: "Bash"}) // doesn't implement ReadOnlyAware
	gate := CreateAutoMemCanUseTool(t.TempDir(), reg)
	ok, reason := gate(context.Background(), "Bash", nil)
	if ok {
		t.Fatalf("Bash without ReadOnlyAware should fail closed")
	}
	if !strings.Contains(reason, "shell tools are disabled") {
		t.Errorf("got %q", reason)
	}
}

func TestAutoMemGate_EditWriteWithinMemdir(t *testing.T) {
	root := t.TempDir()
	gate := CreateAutoMemCanUseTool(root, tools.NewRegistry())
	for _, name := range []string{"Edit", "Write"} {
		ok, reason := gate(context.Background(), name, map[string]any{
			"file_path": filepath.Join(root, "user_role.md"),
		})
		if !ok {
			t.Errorf("%s within memdir should be allowed: %s", name, reason)
		}
	}
}

func TestAutoMemGate_EditWriteAcceptsPathField(t *testing.T) {
	root := t.TempDir()
	gate := CreateAutoMemCanUseTool(root, tools.NewRegistry())
	ok, _ := gate(context.Background(), "Write", map[string]any{
		"path": filepath.Join(root, "x.md"),
	})
	if !ok {
		t.Fatalf("'path' field should also work for path lookup")
	}
}

func TestAutoMemGate_WriteOutsideMemdirDenied(t *testing.T) {
	root := t.TempDir()
	gate := CreateAutoMemCanUseTool(root, tools.NewRegistry())
	ok, reason := gate(context.Background(), "Write", map[string]any{
		"file_path": "/etc/passwd",
	})
	if ok {
		t.Fatalf("Write to /etc/passwd must be denied")
	}
	if !strings.Contains(reason, "outside memdir root") {
		t.Errorf("reason missing scope info: %q", reason)
	}
}

func TestAutoMemGate_WriteViaSymlinkedMissingParentDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	root := t.TempDir()
	outside := t.TempDir()
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	gate := CreateAutoMemCanUseTool(root, tools.NewRegistry())
	target := filepath.Join(alias, "missing", "memory.md")
	if ok, reason := gate(context.Background(), "Write", map[string]any{"file_path": target}); ok || !strings.Contains(reason, "outside memdir") {
		t.Fatalf("symlink escape = (%v, %q), want outside-memdir deny", ok, reason)
	}
}

func TestAutoMemGate_WriteWithoutPathDenied(t *testing.T) {
	root := t.TempDir()
	gate := CreateAutoMemCanUseTool(root, tools.NewRegistry())
	ok, reason := gate(context.Background(), "Edit", map[string]any{})
	if ok {
		t.Fatalf("Edit without file_path must be denied")
	}
	if !strings.Contains(reason, "missing file_path") {
		t.Errorf("got %q", reason)
	}
}

func TestAutoMemGate_UnknownToolDenied(t *testing.T) {
	gate := CreateAutoMemCanUseTool(t.TempDir(), tools.NewRegistry())
	ok, reason := gate(context.Background(), "DangerousMystery", nil)
	if ok {
		t.Fatalf("default-deny should reject unknown tools")
	}
	if !strings.Contains(reason, "not in whitelist") {
		t.Errorf("got %q", reason)
	}
}

var _ pubtool.ReadOnlyAware = fakeBashTool{}
