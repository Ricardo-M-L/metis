package main

import (
	"context"
	"strings"
	"testing"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
)

func TestPromptContextFromRuntimeDetectsConfiguredComputerUse(t *testing.T) {
	reg := &mcp.Registry{Servers: []mcp.ServerEntry{{Name: mcp.ReservedComputerUseName}}}
	ctx := promptContextFromRuntime("model", "provider", "default", reg, true)
	if !ctx.ComputerUseAvailable {
		t.Fatal("configured computer-use server was not propagated into prompt context")
	}
	if section := rtpkg.ComputerUseSection(ctx); section.Name != "computer_use" {
		t.Fatalf("bootstrap prompt context did not activate computer-use section: %q", section.Name)
	}

	reg.Servers[0].Disabled = true
	ctx = promptContextFromRuntime("model", "provider", "default", reg, true)
	if ctx.ComputerUseAvailable {
		t.Fatal("disabled computer-use server should not activate prompt capability")
	}
}

func TestSetupRuntimePromptMatchesFinalToolVisibility(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tools       string
		wantWebText bool
	}{
		{name: "read-only", tools: "Read", wantWebText: false},
		{name: "web-search", tools: "WebSearch", wantWebText: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			t.Setenv("ANTHROPIC_API_KEY", "test-key")
			t.Setenv("METIS_AUTO_MEMORY", "0")
			rt, err := setupRuntime(context.Background(), &cliFlags{
				bare: true, noAuthWizard: true, tools: tc.tools,
			})
			if err != nil {
				t.Fatalf("setupRuntime: %v", err)
			}
			defer rt.Cleanup()
			hasWebText := strings.Contains(rt.loop.System, "WebSearch") || strings.Contains(rt.loop.System, "webReader")
			if hasWebText != tc.wantWebText {
				t.Fatalf("tools=%q web routing present=%v, want %v\n%s", tc.tools, hasWebText, tc.wantWebText, rt.loop.System)
			}
		})
	}
}
