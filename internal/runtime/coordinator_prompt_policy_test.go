package runtime

import (
	"strings"
	"testing"
)

// The fallback must retain the same scope boundaries as the bundled profile:
// an ordinary team request must not silently become an implementation project.
func TestCoordinatorPromptsKeepSessionWorkOutOfDurableScaffold(t *testing.T) {
	profile, err := LoadBuiltinProfile("coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if profile == nil {
		t.Fatal("bundled coordinator profile is missing")
	}
	for name, body := range map[string]string{
		"profile":  profile.SystemPrompt,
		"fallback": defaultCoordinatorOverlay,
	} {
		t.Run(name, func(t *testing.T) {
			body = strings.Join(strings.Fields(body), " ")
			for _, boundary := range []string{
				"Use session-local orchestration by default",
				"read-only research, reviews, sub-agent naming tests, and UI smoke tests",
				"do not create a ProjectCoordinator run",
				"spawn exactly N sub-agents",
				"preserve the requested names",
				"only when the user explicitly requests cross-session persistence or recovery",
				"Default graph stages do not authorize implementation",
				"model-written PASS cannot replace host acceptance checks",
			} {
				if !strings.Contains(body, boundary) {
					t.Errorf("coordinator scope policy is missing %q", boundary)
				}
			}
		})
	}
}

func TestInteractionModesKeepReadOnlyRequestsReadOnly(t *testing.T) {
	body := strings.Join(strings.Fields(AssembleBaseString(PromptCtx{})), " ")
	for _, boundary := range []string{
		"implement and verify only requested changes",
		"Read-only requests end with findings, without state changes",
	} {
		if !strings.Contains(body, boundary) {
			t.Errorf("interaction mode scope policy is missing %q", boundary)
		}
	}
}
