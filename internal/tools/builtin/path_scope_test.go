package builtin

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestPathAwareToolsPassConcreteTargetToGate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		wantPath string
		call     func(*permission.Gate) (tools.Permission, string)
	}{
		{name: "Read", wantPath: "/outside/file", call: func(g *permission.Gate) (tools.Permission, string) {
			return (Read{gate: g}).CanUse(context.Background(), map[string]any{"path": "/outside/file"})
		}},
		{name: "LS", wantPath: "/outside/dir", call: func(g *permission.Gate) (tools.Permission, string) {
			return (LS{gate: g}).CanUse(context.Background(), map[string]any{"path": "/outside/dir"})
		}},
		{name: "Glob default root", wantPath: ".", call: func(g *permission.Gate) (tools.Permission, string) {
			return (Glob{gate: g}).CanUse(context.Background(), map[string]any{"pattern": "**/*.go"})
		}},
		{name: "Grep explicit root", wantPath: "/outside/tree", call: func(g *permission.Gate) (tools.Permission, string) {
			return (Grep{gate: g}).CanUse(context.Background(), map[string]any{"root": "/outside/tree", "pattern": "token"})
		}},
		{name: "Edit", wantPath: "/outside/file", call: func(g *permission.Gate) (tools.Permission, string) {
			return (Edit{gate: g}).CanUse(context.Background(), map[string]any{"path": "/outside/file"})
		}},
		{name: "Write", wantPath: "/outside/file", call: func(g *permission.Gate) (tools.Permission, string) {
			return (Write{gate: g}).CanUse(context.Background(), map[string]any{"path": "/outside/file"})
		}},
		{name: "NotebookEdit", wantPath: "/outside/book.ipynb", call: func(g *permission.Gate) (tools.Permission, string) {
			return (NotebookEdit{gate: g}).CanUse(context.Background(), map[string]any{"path": "/outside/book.ipynb"})
		}},
		{name: "ViewImage", wantPath: "../outside/image.png", call: func(g *permission.Gate) (tools.Permission, string) {
			return (ViewImage{gate: g}).CanUse(context.Background(), map[string]any{"path": "../outside/image.png"})
		}},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := permission.New(permission.ModeAcceptEdits)
			g.SetReadOnlyHook(func(tool, _ string) bool {
				switch tool {
				case "Read", "LS", "Glob", "Grep", "ViewImage":
					return true
				default:
					return false
				}
			})
			var gotPath string
			g.SetPathScopeHook(func(path string) bool {
				gotPath = path
				return false
			})

			got, source := tc.call(g)
			if got != tools.PermissionAsk || source != "scope:outside" {
				t.Fatalf("permission = %v (%s), want out-of-scope ask", got, source)
			}
			if gotPath != tc.wantPath {
				t.Fatalf("scope hook path = %q, want %q", gotPath, tc.wantPath)
			}
		})
	}
}
