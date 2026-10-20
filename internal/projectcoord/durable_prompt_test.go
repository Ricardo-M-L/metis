package projectcoord

import (
	"strings"
	"testing"
)

func TestWorkerPromptExplainsActualForegroundReceiptPolicy(t *testing.T) {
	prompt := BuildWorkerPrompt(Run{Workspace: "/project", Goal: "bounded work"}, WorkItem{ID: "work", Subject: "check", Prompt: "run acceptance", Phase: PhaseVerification})
	for _, rule := range []string{"finish in the foreground", "including finite tests", "run_in_background, await_completion and required_for_completion", "general background-job guidance does not apply", "timeout_ms", "uncertain operation"} {
		if !strings.Contains(prompt, rule) {
			t.Fatalf("missing coordinator execution rule %q", rule)
		}
	}
}
