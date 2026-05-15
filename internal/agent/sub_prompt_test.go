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

func TestBuildSubPrompt_TeammateAnonymousNoTrailer(t *testing.T) {
	t.Parallel()
	// 2026-05-15 prompt-cache fix: TeammateName is now appended as
	// a trailing "Teammate identity: <name>." line outside the
	// header (so the header byte-prefix stays identical across
	// siblings spawned in the same wave). When name is empty the
	// trailer is omitted entirely. The header itself must still
	// produce a valid teammate-mode prompt — peer-messaging
	// guidance is preserved, just without an identity stamp.
	out := BuildSubPrompt(SubPromptInputs{
		Mode: SubPromptTeammate,
		Base: "BASE",
		// No TeammateName.
	})
	if strings.Contains(out, "Teammate identity:") {
		t.Errorf("anonymous teammate must NOT get an identity trailer; got %q", short(out))
	}
	if !strings.Contains(out, "peer_message") {
		t.Errorf("teammate header should still mention peer_message; got %q", short(out))
	}
	if !strings.Contains(out, "MessageTeammate") {
		t.Errorf("teammate header should still mention MessageTeammate; got %q", short(out))
	}
}

func TestBuildSubPrompt_TeammateNamePlacedAtEndForCacheReuse(t *testing.T) {
	t.Parallel()
	// Pin the cache-reuse invariant: two siblings with the SAME
	// profile/mode but DIFFERENT names must share a long byte-
	// identical prefix (everything up to "Teammate identity:").
	// Pre-fix the name was inlined in the header's first sentence,
	// busting the prefix at character ~13.
	a := BuildSubPrompt(SubPromptInputs{
		Mode:         SubPromptTeammate,
		Base:         "SHARED-BASE-CONTENT-25K-EQUIVALENT",
		TeammateName: "P1",
	})
	b := BuildSubPrompt(SubPromptInputs{
		Mode:         SubPromptTeammate,
		Base:         "SHARED-BASE-CONTENT-25K-EQUIVALENT",
		TeammateName: "P2",
	})

	// Find the longest common prefix.
	common := 0
	for common < len(a) && common < len(b) && a[common] == b[common] {
		common++
	}
	// Both prompts include the full header + base. Names diverge
	// only in the trailing identity line. Common prefix MUST cover
	// the base content (>= len of "SHARED-BASE-CONTENT-25K-EQUIVALENT").
	if common < len("SHARED-BASE-CONTENT-25K-EQUIVALENT") {
		t.Errorf("common prefix = %d bytes, want >= base length (cache-reuse invariant broken). a[:200]=%q b[:200]=%q",
			common, short(a), short(b))
	}
	// Sanity: both should still mention their own name somewhere.
	if !strings.Contains(a, "P1") || !strings.Contains(b, "P2") {
		t.Errorf("names lost: a contains P1=%v, b contains P2=%v", strings.Contains(a, "P1"), strings.Contains(b, "P2"))
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
