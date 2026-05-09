package agent

import (
	"context"
	"path/filepath"
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

func TestAutoMemGate_BashReadOnlyAllowed(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(fakeBashTool{forkFakeTool: forkFakeTool{name: "Bash"}, readOnly: true})
	gate := CreateAutoMemCanUseTool(t.TempDir(), reg)
	ok, reason := gate(context.Background(), "Bash", map[string]any{"command": "ls"})
	if !ok {
		t.Fatalf("read-only Bash should be allowed, got: %s", reason)
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
	if !strings.Contains(reason, "read-only") {
		t.Errorf("reason missing 'read-only': %q", reason)
	}
}

func TestAutoMemGate_BashUnregisteredDenied(t *testing.T) {
	gate := CreateAutoMemCanUseTool(t.TempDir(), tools.NewRegistry())
	ok, reason := gate(context.Background(), "Bash", nil)
	if ok {
		t.Fatalf("unregistered Bash should fail closed")
	}
	if !strings.Contains(reason, "not registered") {
		t.Errorf("reason should mention not-registered: %q", reason)
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
	if !strings.Contains(reason, "fail-closed") {
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
