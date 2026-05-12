package agent

// sub_prompt_test.go — locks Phase G.13 (2026-05-12) sub-agent prompt
// template contracts:
//
//   1. Fork mode header reminds the child it can see parent history.
//   2. Agent mode header is the "focused sub-task" framing.
//   3. Teammate mode includes the peer-message + MessageTeammate
//      affordance.
//   4. Empty TeammateName falls back to a neutral pronoun ("this
//      teammate"), no panic.
//   5. Base + ProfileSystemPrompt both flow through into the joined
//      output in the right order.
//   6. Unknown Mode returns the Base unchanged (defensive).

import (
	"strings"
	"testing"
)

// short truncates s for assertion messages.
func short(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

func TestBuildSubPrompt_ForkHeader(t *testing.T) {
	t.Parallel()
	out := BuildSubPrompt(SubPromptInputs{Mode: SubPromptFork, Base: "BASE"})
	if !strings.Contains(out, "forked sub-agent") {
		t.Errorf("Fork header missing role; got %q", short(out))
	}
	if !strings.Contains(out, "ALREADY READ") {
		t.Errorf("Fork header should remind child not to re-summarize; got %q", short(out))
	}
	if !strings.Contains(out, "BASE") {
		t.Errorf("Base not in output: %q", short(out))
	}
}

func TestBuildSubPrompt_AgentHeader(t *testing.T) {
	t.Parallel()
	out := BuildSubPrompt(SubPromptInputs{Mode: SubPromptAgent, Base: "BASE"})
	if !strings.Contains(out, "focused sub-agent") {
		t.Errorf("Agent header missing role; got %q", short(out))
	}
	if !strings.Contains(out, "DO NOT see the parent") {
		t.Errorf("Agent header should clarify cold start; got %q", short(out))
	}
}

func TestBuildSubPrompt_TeammateNamed(t *testing.T) {
	t.Parallel()
	out := BuildSubPrompt(SubPromptInputs{
		Mode:         SubPromptTeammate,
		Base:         "BASE",
		TeammateName: "alice",
	})
	if !strings.Contains(out, "alice") {
		t.Errorf("named teammate header missing name; got %q", short(out))
	}
	if !strings.Contains(out, "peer_message") {
		t.Errorf("teammate header should mention peer_message reminder; got %q", short(out))
	}
	if !strings.Contains(out, "MessageTeammate") {
		t.Errorf("teammate header should mention MessageTeammate tool; got %q", short(out))
	}
}

func TestBuildSubPrompt_TeammateAnonymousFallsBack(t *testing.T) {
	t.Parallel()
	out := BuildSubPrompt(SubPromptInputs{
		Mode: SubPromptTeammate,
		Base: "BASE",
		// No TeammateName.
	})
	if !strings.Contains(out, "this teammate") {
		t.Errorf("missing name should use 'this teammate' fallback; got %q", short(out))
	}
}

func TestBuildSubPrompt_BaseAndProfileFlow(t *testing.T) {
	t.Parallel()
	out := BuildSubPrompt(SubPromptInputs{
		Mode:                SubPromptAgent,
		Base:                "BASE_BODY",
		ProfileSystemPrompt: "PROFILE_BODY",
	})
	if !strings.Contains(out, "BASE_BODY") || !strings.Contains(out, "PROFILE_BODY") {
		t.Errorf("output should join header+base+profile; got %q", short(out))
	}
	// Profile should come AFTER base (more specific wins).
	baseIdx := strings.Index(out, "BASE_BODY")
	profIdx := strings.Index(out, "PROFILE_BODY")
	if baseIdx < 0 || profIdx < 0 || profIdx <= baseIdx {
		t.Errorf("profile should follow base in output: base@%d profile@%d", baseIdx, profIdx)
	}
}

func TestBuildSubPrompt_UnknownModeIsSafe(t *testing.T) {
	t.Parallel()
	out := BuildSubPrompt(SubPromptInputs{
		Mode: SubPromptMode(999),
		Base: "BASE",
	})
	if !strings.Contains(out, "BASE") {
		t.Errorf("unknown mode should still surface Base; got %q", short(out))
	}
}

func TestBuildSubPrompt_EmptyBaseAndProfileProducesHeaderOnly(t *testing.T) {
	t.Parallel()
	out := BuildSubPrompt(SubPromptInputs{Mode: SubPromptAgent})
	if out == "" {
		t.Error("empty Base + Profile should still produce a header")
	}
	if !strings.Contains(out, "focused sub-agent") {
		t.Errorf("header missing; got %q", short(out))
	}
}
