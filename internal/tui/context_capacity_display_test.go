package tui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestRenderContextUnknownWindowDoesNotClaimCapacityOrOccupancy(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	for _, prompt := range []string{"", strings.Repeat("uninitialized context ", 100)} {
		provider := &changingContextProvider{}
		m := &Model{loop: &agent.Loop{Provider: provider}, model: "uninitialized-model"}
		if prompt != "" {
			m.loop.AppendUser(prompt)
		}
		used := m.loop.ContextDisplaySnapshot().Used
		out := stripANSI(renderContext(m))
		if !strings.Contains(out, "window unknown") || !strings.Contains(out, fmtThousands(used)+" tokens used") {
			t.Errorf("unknown capacity is unclear: %s", out)
		}
		if strings.Contains(out, "%") || strings.Contains(out, "/0 tokens") || strings.ContainsAny(out, "⛁⛶") {
			t.Errorf("unknown capacity invented a denominator or occupancy grid: %s", out)
		}
		if provider.calls != 0 {
			t.Errorf("render queried provider capacity %d times", provider.calls)
		}
	}
}

func TestRenderContextKnownWindowUsesCanonicalSnapshot(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	m := minimalModel(128000)
	setCanonicalContextTokens(t, m, 38000)
	out := stripANSI(renderContext(m))
	if !strings.Contains(out, "128,000 window") || !strings.Contains(out, "38,000/128,000 tokens (29%)") {
		t.Fatalf("known capacity did not render canonical occupancy: %s", out)
	}
	if strings.Contains(out, "window unknown") || !strings.Contains(out, "⛁") || !strings.Contains(out, "⛶") {
		t.Fatalf("known capacity grid is missing: %s", out)
	}
}
