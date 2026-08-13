package slash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitCommandDelegatesRepositoryAnalysisToTheAgent(t *testing.T) {
	repo := t.TempDir()
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalWD) })

	registry := NewRegistry()
	RegisterInitCommand(registry)
	handled, prompt, signal, _ := registry.Parse("/init")
	if !handled {
		t.Fatal("/init was not registered")
	}
	if signal != SignalCustomPrompt {
		t.Fatalf("/init signal = %v, want SignalCustomPrompt", signal)
	}
	lowerPrompt := strings.ToLower(prompt)
	for _, want := range []string{
		"analyze this repository",
		"claude.md",
		"build",
		"test",
		"do not invent",
	} {
		if !strings.Contains(lowerPrompt, want) {
			t.Errorf("/init prompt missing %q:\n%s", want, prompt)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatalf("/init handler wrote CLAUDE.md before the agent ran: %v", err)
	}
}

func TestInitCommandPreservesOptionalUserFocus(t *testing.T) {
	registry := NewRegistry()
	RegisterInitCommand(registry)

	_, prompt, signal, _ := registry.Parse("/init focus on release workflows")
	if signal != SignalCustomPrompt {
		t.Fatalf("signal = %v, want SignalCustomPrompt", signal)
	}
	if !strings.Contains(prompt, "focus on release workflows") {
		t.Fatalf("user focus missing from prompt:\n%s", prompt)
	}
}
