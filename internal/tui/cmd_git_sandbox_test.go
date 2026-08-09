package tui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

func TestGitShortcutFailsClosedWhenSandboxUnavailable(t *testing.T) {
	manager, err := sandbox.NewManager("permissions")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	out := cmdGitStatus(&REPL{sandbox: manager}, "")
	if !strings.Contains(out, "sandbox failed closed") {
		t.Fatalf("git shortcut bypassed closed sandbox: %q", out)
	}
}

func TestExtractFlagKeepsQuotedMultiwordCommitMessage(t *testing.T) {
	if got := extractFlag(`-m "fix slash routing"`, "-m"); got != "fix slash routing" {
		t.Fatalf("extractFlag=%q", got)
	}
	if got := extractFlag("-m '修复 命令 路由'", "-m"); got != "修复 命令 路由" {
		t.Fatalf("extractFlag CJK=%q", got)
	}
}
