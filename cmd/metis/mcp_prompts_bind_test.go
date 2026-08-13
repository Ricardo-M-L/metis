package main

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/slash"
)

func TestRegisterMCPPromptPreservesCatalogMetadata(t *testing.T) {
	r := slash.NewRegistry()
	registered := registerMCPPromptsAsSlash(r, []mcp.PromptHandle{{
		SlashName:   "mcp__github__pr_summary",
		ServerName:  "github",
		PromptName:  "pr_summary",
		Description: "summarize a pull request",
		Arguments:   []string{"number", "tone*"},
	}})
	if len(registered) != 1 || registered[0] != "mcp__github__pr_summary" {
		t.Fatalf("registered names = %v", registered)
	}
	cmd, ok := r.Resolve("mcp__github__pr_summary")
	if !ok {
		t.Fatal("MCP prompt missing from slash registry")
	}
	if cmd.Description != "summarize a pull request" || cmd.ArgumentHint != "number tone*" ||
		cmd.Source != "mcp:github" || cmd.Category != "mcp" || !cmd.IsVisible() || !cmd.IsEnabled() {
		t.Fatalf("MCP catalog metadata = %+v", cmd)
	}
}
