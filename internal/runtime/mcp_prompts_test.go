package runtime

// Phase-D #40 tests — exercise the pure-string helpers (description
// formatting, arg name flattening). The actual collector talks to a
// live MCP server and is covered by the runtime smoke that exists for
// real launches.

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/mcp"
)

func TestArgNames_FlagsOptional(t *testing.T) {
	args := []mcp.PromptArgument{
		{Name: "repo", Required: true},
		{Name: "limit", Required: false},
	}
	got := argNames(args)
	if len(got) != 2 {
		t.Fatalf("len mismatch: %v", got)
	}
	if got[0] != "repo" {
		t.Errorf("required should not have * suffix; got %q", got[0])
	}
	if got[1] != "limit*" {
		t.Errorf("optional should have * suffix; got %q", got[1])
	}
}

func TestPromptDescription_DefaultWhenEmpty(t *testing.T) {
	h := &MCPPromptHandle{
		SlashName:  "mcp__github__pr_summary",
		ServerName: "github",
		PromptName: "pr_summary",
	}
	got := h.PromptDescription()
	if !strings.Contains(got, "github") {
		t.Errorf("default description should reference server name; got: %q", got)
	}
}

func TestPromptDescription_WithArgsListed(t *testing.T) {
	h := &MCPPromptHandle{
		SlashName:   "mcp__github__pr_summary",
		ServerName:  "github",
		PromptName:  "pr_summary",
		Description: "Summarize a PR",
		Arguments:   []string{"number", "tone*"},
	}
	got := h.PromptDescription()
	if !strings.Contains(got, "args: number, tone*") {
		t.Errorf("arg list missing/wrong; got: %q", got)
	}
	if !strings.Contains(got, "Summarize a PR") {
		t.Errorf("user-supplied description should appear; got: %q", got)
	}
}
