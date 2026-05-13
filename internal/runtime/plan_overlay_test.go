package runtime

import (
	"strings"
	"testing"
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
	if !got.Cache {
		t.Error("plan overlay should be cacheable (Cache=true) — it's stable for the session")
	}
	if got.Volatile {
		t.Error("plan overlay should not be Volatile")
	}
	for _, must := range []string{"read-only", "Allowed tools", "Read, LS, Glob, Grep", "Do NOT", "TodoWrite"} {
		if !strings.Contains(got.Body, must) {
			t.Errorf("plan overlay body missing %q; got:\n%s", must, got.Body)
		}
	}
}
