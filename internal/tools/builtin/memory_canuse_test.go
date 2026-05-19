package builtin

// Memory.CanUse is gate-routed — 2026-05-18 audit caught that the
// pre-fix version returned PermissionAllow unconditionally, which
// silently let plan mode rewrite memory blocks. These tests pin the
// gate-routed behavior across the relevant modes.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func newMemoryWithGate(t *testing.T, mode permission.Mode) Memory {
	t.Helper()
	mm, err := memory.NewMemoryManager(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("NewMemoryManager: %v", err)
	}
	g := permission.New(mode)
	return NewMemory(g, mm)
}

func TestMemory_CanUse_PlanModeDenies(t *testing.T) {
	tool := newMemoryWithGate(t, permission.ModePlan)
	for _, action := range []string{"add", "replace", "remove"} {
		perm, _ := tool.CanUse(context.Background(), map[string]any{"action": action})
		if perm != tools.PermissionDeny {
			t.Errorf("plan mode Memory.%s should DENY; got %v", action, perm)
		}
	}
}

func TestMemory_CanUse_AskModePrompts(t *testing.T) {
	tool := newMemoryWithGate(t, permission.ModeAsk)
	perm, _ := tool.CanUse(context.Background(), map[string]any{"action": "add"})
	if perm != tools.PermissionAsk {
		t.Errorf("ask mode Memory.add should ASK; got %v", perm)
	}
}

func TestMemory_CanUse_BypassAllows(t *testing.T) {
	tool := newMemoryWithGate(t, permission.ModeBypass)
	perm, _ := tool.CanUse(context.Background(), map[string]any{"action": "add"})
	if perm != tools.PermissionAllow {
		t.Errorf("bypass mode Memory.add should ALLOW; got %v", perm)
	}
}

func TestMemory_CanUse_DenyModeRejects(t *testing.T) {
	tool := newMemoryWithGate(t, permission.ModeDeny)
	perm, _ := tool.CanUse(context.Background(), map[string]any{"action": "add"})
	if perm != tools.PermissionDeny {
		t.Errorf("deny mode Memory.add should DENY; got %v", perm)
	}
}
