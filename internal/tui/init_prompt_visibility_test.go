package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestInitKeepsAgentInstructionsOutOfVisibleHistory(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)

	m := newSlashTestModelWithLoop(t)
	m.input.SetValue("/init focus on release workflows")
	pressEnter(t, m)
	if cancel, ok := m.ctx.Value(cancelKey{}).(context.CancelFunc); ok {
		cancel()
	}
	time.Sleep(50 * time.Millisecond)

	for _, message := range m.messages {
		if strings.Contains(message.Content, "# Initialize repository guidance") ||
			strings.Contains(message.Content, internalReviewPromptOpen) {
			t.Fatalf("internal /init frame leaked into live transcript: %+v", message)
		}
	}
	foundVisible := false
	for _, message := range m.messages {
		if message.Role == "user" && message.Content == "/init focus on release workflows" {
			foundVisible = true
		}
	}
	if !foundVisible {
		t.Fatalf("visible /init invocation missing: %+v", m.messages)
	}

	history := m.loop.History()
	providerPromptFound := false
	for _, message := range history {
		if message.Role != provider.RoleUser {
			continue
		}
		for _, block := range message.Content {
			if strings.Contains(block.Text, "# Initialize repository guidance") &&
				strings.Contains(block.Text, internalReviewPromptOpen) &&
				strings.Contains(block.Text, "focus on release workflows") {
				providerPromptFound = true
			}
		}
	}
	if !providerPromptFound {
		t.Fatalf("provider-facing /init frame missing from durable history: %+v", history)
	}
	if _, prefill, ok := transcript.UndoWithPrefill(history); !ok || prefill != "/init focus on release workflows" {
		t.Fatalf("/init undo prefill = %q, %v; want visible invocation", prefill, ok)
	}

	exported := conversationText(history)
	if !strings.Contains(exported, "❯ /init focus on release workflows") ||
		strings.Contains(exported, "# Initialize repository guidance") ||
		strings.Contains(exported, internalReviewPromptOpen) {
		t.Fatalf("/init export crossed visibility boundary:\n%s", exported)
	}
	assertInitDidNotWriteDirectly(t, project)
}

func TestPlainREPLInitUsesTheSameVisibilityBoundary(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)

	registry := slash.NewRegistry()
	slash.RegisterAll(registry, &config.Config{})
	r, out := newPromptTestREPL("/init focus on release workflows\n/quit\n", registry)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	stdout := stripANSI(out.String())
	if strings.Contains(stdout, "# Initialize repository guidance") || strings.Contains(stdout, internalReviewPromptOpen) {
		t.Fatalf("internal /init frame leaked to plain REPL stdout:\n%s", stdout)
	}
	history := r.Loop.History()
	exported := conversationText(history)
	if !strings.Contains(exported, "❯ /init focus on release workflows") ||
		strings.Contains(exported, "# Initialize repository guidance") {
		t.Fatalf("plain REPL /init export crossed visibility boundary:\n%s", exported)
	}
	assertInitDidNotWriteDirectly(t, project)
}

func assertInitDidNotWriteDirectly(t *testing.T, project string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(project, "CLAUDE.md"))
	if !os.IsNotExist(err) {
		t.Fatalf("canonical /init wrote CLAUDE.md before the model/tool flow ran: %v", err)
	}
}
