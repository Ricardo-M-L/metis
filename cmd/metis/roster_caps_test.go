package main

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// fresh returns a Config with only the Agents section populated, so
// individual layers can be tested without unrelated default noise.
func fresh(named, anon, legacy int) *config.Config {
	return &config.Config{
		Agents: config.Agents{
			MaxConcurrentNamed:     named,
			MaxConcurrentAnon:      anon,
			MaxConcurrentSubAgents: legacy,
		},
	}
}

func TestResolveRosterCaps_DefaultsWhenAllZero(t *testing.T) {
	t.Setenv("METIS_MAX_SUBAGENTS", "")
	t.Setenv("METIS_MAX_SUBAGENTS_NAMED", "")
	t.Setenv("METIS_MAX_SUBAGENTS_ANON", "")
	named, anon := resolveRosterCaps(fresh(0, 0, 0))
	if named != 20 || anon != 40 {
		t.Errorf("default caps want (20,40); got (%d,%d)", named, anon)
	}
}

func TestResolveRosterCaps_LegacyCombinedSplits1to2(t *testing.T) {
	t.Setenv("METIS_MAX_SUBAGENTS", "")
	t.Setenv("METIS_MAX_SUBAGENTS_NAMED", "")
	t.Setenv("METIS_MAX_SUBAGENTS_ANON", "")
	// Legacy cap=30 → ceil(30/3)=10 named, 20 anon.
	named, anon := resolveRosterCaps(fresh(0, 0, 30))
	if named != 10 || anon != 20 {
		t.Errorf("legacy 30 → 10/20 split; got (%d,%d)", named, anon)
	}
}

func TestResolveRosterCaps_NewFieldsOverrideLegacy(t *testing.T) {
	t.Setenv("METIS_MAX_SUBAGENTS", "")
	t.Setenv("METIS_MAX_SUBAGENTS_NAMED", "")
	t.Setenv("METIS_MAX_SUBAGENTS_ANON", "")
	// Legacy says 30 but new explicit fields take precedence.
	named, anon := resolveRosterCaps(fresh(7, 13, 30))
	if named != 7 || anon != 13 {
		t.Errorf("new fields should override legacy; got (%d,%d)", named, anon)
	}
}

func TestResolveRosterCaps_EnvCombinedOverridesToml(t *testing.T) {
	t.Setenv("METIS_MAX_SUBAGENTS", "60")
	t.Setenv("METIS_MAX_SUBAGENTS_NAMED", "")
	t.Setenv("METIS_MAX_SUBAGENTS_ANON", "")
	// 60 → 20 named, 40 anon. New toml fields explicitly 5/5 get overridden.
	named, anon := resolveRosterCaps(fresh(5, 5, 0))
	if named != 20 || anon != 40 {
		t.Errorf("env combined 60 should split to 20/40 over toml 5/5; got (%d,%d)", named, anon)
	}
}

func TestResolveRosterCaps_EnvNamedOverridesEverything(t *testing.T) {
	t.Setenv("METIS_MAX_SUBAGENTS", "60")
	t.Setenv("METIS_MAX_SUBAGENTS_NAMED", "99")
	t.Setenv("METIS_MAX_SUBAGENTS_ANON", "")
	named, anon := resolveRosterCaps(fresh(5, 5, 0))
	if named != 99 {
		t.Errorf("METIS_MAX_SUBAGENTS_NAMED should override combined env; got named=%d want 99", named)
	}
	if anon != 40 {
		t.Errorf("METIS_MAX_SUBAGENTS_ANON unset should leave anon at combined-env split; got %d want 40", anon)
	}
}

func TestResolveRosterCaps_EnvAnonStandalone(t *testing.T) {
	t.Setenv("METIS_MAX_SUBAGENTS", "")
	t.Setenv("METIS_MAX_SUBAGENTS_NAMED", "")
	t.Setenv("METIS_MAX_SUBAGENTS_ANON", "50")
	named, anon := resolveRosterCaps(fresh(0, 0, 0))
	if named != 20 {
		t.Errorf("anon-only env should leave named at default 20; got %d", named)
	}
	if anon != 50 {
		t.Errorf("METIS_MAX_SUBAGENTS_ANON=50 should set anon=50; got %d", anon)
	}
}

func TestResolveRosterCaps_NonNumericEnvIgnored(t *testing.T) {
	t.Setenv("METIS_MAX_SUBAGENTS", "")
	t.Setenv("METIS_MAX_SUBAGENTS_NAMED", "garbage")
	t.Setenv("METIS_MAX_SUBAGENTS_ANON", "")
	named, anon := resolveRosterCaps(fresh(0, 0, 0))
	if named != 20 || anon != 40 {
		t.Errorf("non-numeric env should be ignored, defaults preserved; got (%d,%d)", named, anon)
	}
}

func TestSplitOneToTwo_Rounding(t *testing.T) {
	cases := []struct {
		total        int
		wantN, wantA int
	}{
		{0, 0, 0}, // unlimited propagates
		{3, 1, 2},
		{6, 2, 4},
		{30, 10, 20},
		{60, 20, 40},
		{31, 11, 20}, // ceil(31/3) = 11, anon = 31-11 = 20
		{5, 2, 3},    // ceil(5/3) = 2, anon = 3
	}
	for _, c := range cases {
		n, a := splitOneToTwo(c.total)
		if n != c.wantN || a != c.wantA {
			t.Errorf("splitOneToTwo(%d) = (%d,%d); want (%d,%d)", c.total, n, a, c.wantN, c.wantA)
		}
	}
}
