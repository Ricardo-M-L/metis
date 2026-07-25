package runtime

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
)

func TestPlanOverlay_InactiveReturnsZero(t *testing.T) {
	got := PlanOverlay(false)
	if got.Name != "" {
		t.Errorf("inactive plan should produce zero-value section, got Name=%q", got.Name)
	}
	if got.Body != "" {
		t.Errorf("inactive plan should have empty body, got %q", got.Body)
	}
}

func TestPlanOverlay_ActiveProducesPlanModeSection(t *testing.T) {
	got := PlanOverlay(true)
	if got.Name != "plan_mode" {
		t.Errorf("active plan should be named plan_mode, got %q", got.Name)
	}
	if got.Cache {
		t.Error("dynamic plan overlay must not be cacheable")
	}
	if !got.Volatile {
		t.Error("dynamic plan overlay must be Volatile")
	}
	for _, must := range []string{
		"live permission mode is plan",
		"read-only",
		"Read, LS, Glob, Grep, WebFetch",
		"Agent",
		"AskUser",
		"ExitPlanMode",
		"only approval path",
		"no longer apply",
	} {
		if !strings.Contains(got.Body, must) {
			t.Errorf("plan overlay body missing %q; got:\n%s", must, got.Body)
		}
	}
}

func TestPlanOverlay_DoesNotAdvertiseUnavailableOrLegacyExitPaths(t *testing.T) {
	body := PlanOverlay(true).Body
	for _, forbidden := range []string{
		"/auto",
		"/bypass",
		"STOP and wait",
		"final assistant message",
		"Agent / Fork",
		"Use TodoWrite",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("dynamic plan overlay must not contain legacy guidance %q; got:\n%s", forbidden, body)
		}
	}
	if !strings.Contains(body, "Fork and TodoWrite are not") {
		t.Fatalf("overlay must accurately explain unavailable plan tools; got:\n%s", body)
	}
}

func TestPlanOverlay_IsStatelessAcrossLiveModeChanges(t *testing.T) {
	if got := PlanOverlay(false); got.Name != "" || got.Body != "" {
		t.Fatalf("inactive request unexpectedly retained plan overlay: %+v", got)
	}
	if got := PlanOverlay(true); got.Name != "plan_mode" || got.Body == "" {
		t.Fatalf("active request did not receive plan overlay: %+v", got)
	}
	if got := PlanOverlay(false); got.Name != "" || got.Body != "" {
		t.Fatalf("overlay leaked after live mode exited plan: %+v", got)
	}
}

func TestRemoveLegacyPlanOverlay_ExactSectionAndGap(t *testing.T) {
	in := "base instructions\n\n" + legacyPlanOverlayBodyV1 + "\n\n<env>live</env>"
	want := "base instructions\n\n<env>live</env>"
	if got := RemoveLegacyPlanOverlay(in); got != want {
		t.Fatalf("RemoveLegacyPlanOverlay() = %q, want %q", got, want)
	}
}

func TestRemoveLegacyPlanOverlay_RemovesOwnedCacheBoundary(t *testing.T) {
	for _, marker := range []string{
		anthropic.SystemPromptCacheBoundary,
		anthropic.SystemPromptCacheBoundary2,
	} {
		t.Run(marker, func(t *testing.T) {
			in := "base\n\n" + legacyPlanOverlayBodyV1 + "\n\n" + marker + "\n\n<env>live</env>"
			want := "base\n\n<env>live</env>"
			if got := RemoveLegacyPlanOverlay(in); got != want {
				t.Fatalf("RemoveLegacyPlanOverlay() = %q, want %q", got, want)
			}
		})
	}
}

func TestRemoveLegacyPlanOverlay_RemovesSurroundingRenderedBoundaries(t *testing.T) {
	in := RenderSections([]SystemPromptSection{
		{Name: "base", Body: "base", Cache: true},
		{Name: "plan_mode", Body: legacyPlanOverlayBodyV1, Cache: true},
		{Name: "env", Body: "<env>live</env>", Volatile: true},
	})
	if !strings.Contains(in, anthropic.SystemPromptCacheBoundary2+"\n\n"+legacyPlanOverlayBodyV1) {
		t.Fatalf("fixture missing the pre-plan Boundary2 topology: %q", in)
	}
	if !strings.Contains(in, legacyPlanOverlayBodyV1+"\n\n"+anthropic.SystemPromptCacheBoundary) {
		t.Fatalf("fixture missing the post-plan Boundary1 topology: %q", in)
	}

	want := "base\n\n<env>live</env>"
	if got := RemoveLegacyPlanOverlay(in); got != want {
		t.Fatalf("RemoveLegacyPlanOverlay() = %q, want %q", got, want)
	}
}

func TestRemoveLegacyPlanOverlay_OnlySection(t *testing.T) {
	if got := RemoveLegacyPlanOverlay(legacyPlanOverlayBodyV1); got != "" {
		t.Fatalf("legacy-only system should become empty, got %q", got)
	}
}

func TestRemoveLegacyPlanOverlay_DoesNotFuzzyMatchUserText(t *testing.T) {
	cases := []string{
		"user prefix " + legacyPlanOverlayBodyV1 + " user suffix",
		strings.Replace(legacyPlanOverlayBodyV1, "STOP and wait", "pause and wait", 1),
		"# Plan mode — read-only exploration only\n\nMy own shorter instructions.",
		planOverlayBody,
		"ordinary user system prompt",
	}
	for _, in := range cases {
		if got := RemoveLegacyPlanOverlay(in); got != in {
			t.Errorf("user text was modified:\ninput: %q\n got: %q", in, got)
		}
	}
}
