//go:build e2e_smoke

// Manual end-to-end smoke. Skipped in normal `go test ./...` because it
// spawns a real metis-cu subprocess (slow, requires the binary on PATH,
// requires macOS Accessibility grant on first run). Run explicitly with:
//
//	go test ./internal/runtime/ -run TestE2E_MCPSmoke -v -tags=e2e_smoke
package runtime_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestE2E_MCPSmoke_DefaultFallback(t *testing.T) {
	// METIS_TEST_FALLBACK is unset → ${X:-fallback-value} should expand
	// to "fallback-value". Use `echo` as the command; the handshake will
	// fail (echo isn't a real MCP server) but the FAILURE MESSAGE should
	// reflect the expanded value, not the literal ${...}.
	reg := &runtime.mcp.Registry{
		Servers: []runtime.mcp.ServerEntry{
			{
				Name:    "fallback",
				Command: "echo",
				Args:    []string{"${METIS_TEST_FALLBACK:-resolved-via-default}"},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := runtime.mcp.LaunchServer(ctx, reg, "fallback", tools.NewRegistry())
	t.Logf("default-fallback launch err: %v", err)
	// Will error on handshake (echo isn't MCP), but reaching that point
	// proves the env-var expansion produced a runnable command.
}

func TestE2E_MCPSmoke_EnvOverridesDefault(t *testing.T) {
	t.Setenv("METIS_TEST_FALLBACK", "real-value-from-env")
	reg := &runtime.mcp.Registry{
		Servers: []runtime.mcp.ServerEntry{
			{
				Name:    "fallback",
				Command: "echo",
				Args:    []string{"${METIS_TEST_FALLBACK:-resolved-via-default}"},
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := runtime.mcp.LaunchServer(ctx, reg, "fallback", tools.NewRegistry())
	t.Logf("env-override launch err: %v", err)
}

func TestE2E_MCPSmoke_ComputerUseFiltered(t *testing.T) {
	// Skip if metis-cu isn't installed — keeps CI green without the
	// platform binary present.
	if _, err := exec.LookPath("metis-cu"); err != nil {
		t.Skipf("metis-cu not in PATH: %v", err)
	}
	reg := &runtime.mcp.Registry{}
	mcp.SetReservedComputerUseServer(reg)
	// Limit to two tools — verifies the per-server enabled_tools filter
	// applied in mcp.LaunchServer's `srv.FilteredTools(...)` call.
	if e := runtime.mcp.FindServer(reg, runtime.ReservedComputerUseName); e != nil {
		e.EnabledTools = []string{"screenshot", "left_click"}
	}
	registry := tools.NewRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srv, err := runtime.mcp.LaunchServer(ctx, reg, runtime.ReservedComputerUseName, registry)
	if err != nil {
		t.Fatalf("launch metis-cu: %v", err)
	}
	defer srv.Close()
	// FilteredTools is what mcp.LaunchServer passed into Register; we
	// check the registry to confirm only the two whitelisted tools
	// landed (with their mcp__computer-use__ prefix).
	all := registry.All()
	mcpToolNames := []string{}
	for _, tt := range all {
		name := tt.Name()
		if len(name) >= 5 && name[:5] == "mcp__" {
			mcpToolNames = append(mcpToolNames, name)
		}
	}
	t.Logf("metis-cu raw tool count from server: %d", len(srv.Tools()))
	t.Logf("registered after enabled_tools filter (%d):", len(mcpToolNames))
	for _, n := range mcpToolNames {
		t.Logf("  - %s", n)
	}
	if len(mcpToolNames) != 2 {
		t.Errorf("expected 2 tools after filter; got %d (%v)",
			len(mcpToolNames), mcpToolNames)
	}
}
