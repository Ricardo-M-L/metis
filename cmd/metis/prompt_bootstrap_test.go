package main

import (
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
