package webui

import (
	"strings"
	"testing"
)

func TestDesktopModelInputExpandsBatchWithoutChangingOrdinaryPrompts(t *testing.T) {
	ordinary := "inspect this repository"
	if got := desktopModelInput(ordinary); got != ordinary {
		t.Fatalf("ordinary input changed: %q", got)
	}
	got := desktopModelInput("/batch   refactor auth flow")
	for _, want := range []string{"multi-agent orchestration", "refactor auth flow", "PHASE 1", "PHASE 2", "PHASE 3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expanded Desktop /batch missing %q", want)
		}
	}
	if got := desktopModelInput("/BATCH\trefactor cache"); !strings.Contains(got, "refactor cache") {
		t.Fatalf("tab-separated /batch was not expanded: %q", got)
	}
}

func TestDesktopCommandCatalogIncludesBatch(t *testing.T) {
	js, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "{ name: '/batch'") {
		t.Fatal("Desktop command catalog does not expose /batch")
	}
}

func TestExecutionStrategyForStatus(t *testing.T) {
	cases := []struct {
		plans, agents, named int
		want                 string
	}{
		{0, 0, 0, "direct"},
		{3, 0, 0, "planned_single_agent"},
		{3, 2, 0, "parallel_sub_agents"},
		{3, 2, 2, "coordinated_agent_team"},
	}
	for _, tc := range cases {
		if got := executionStrategyForStatus(tc.plans, tc.agents, tc.named); got != tc.want {
			t.Fatalf("strategy(%d,%d,%d) = %q, want %q", tc.plans, tc.agents, tc.named, got, tc.want)
		}
	}
}
