package main

// roster_caps.go — resolves the (named, anon) Roster capacities from
// the four input layers (env > new toml fields > legacy single field
// > defaults). Lives in cmd/metis so it can read os.Getenv directly
// and produce final integers before the agent.Roster gets built.
//
// The legacy `MaxConcurrentSubAgents` field stays honored when set so
// existing users' configs don't silently break — it splits 1/3 named,
// 2/3 anon (rounded up) on the same ratio as the new defaults
// (20/40 = 1/3 named, 2/3 anon).

import (
	"os"
	"strconv"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// resolveRosterCaps walks the precedence chain and returns the final
// (named, anon) caps to hand to agent.NewRoster.
//
// Treats 0 as "unset / fall through"; treats negative as "explicitly
// disable cap" (passed straight through; NewRoster reads <= 0 as
// unlimited). This lets a power user write `MaxConcurrentNamed = -1`
// to remove the cap without juggling sentinel values.
func resolveRosterCaps(cfg *config.Config) (named, anon int) {
	// Layer 4: built-in defaults.
	named, anon = 20, 40

	// Layer 3: legacy combined toml field (split 1:2).
	if legacy := cfg.Agents.MaxConcurrentSubAgents; legacy > 0 {
		named, anon = splitOneToTwo(legacy)
	}

	// Layer 2: new explicit toml fields override legacy.
	if cfg.Agents.MaxConcurrentNamed != 0 {
		named = cfg.Agents.MaxConcurrentNamed
	}
	if cfg.Agents.MaxConcurrentAnon != 0 {
		anon = cfg.Agents.MaxConcurrentAnon
	}

	// Layer 1: env. Combined env splits 1:2; per-kind env wins
	// outright. Per-kind applied AFTER combined so the more
	// specific layer overrides on partial sets.
	if v := parseEnvInt("METIS_MAX_SUBAGENTS"); v != nil {
		named, anon = splitOneToTwo(*v)
	}
	if v := parseEnvInt("METIS_MAX_SUBAGENTS_NAMED"); v != nil {
		named = *v
	}
	if v := parseEnvInt("METIS_MAX_SUBAGENTS_ANON"); v != nil {
		anon = *v
	}
	return named, anon
}

// splitOneToTwo divides a single combined cap into (named, anon)
// using a 1:2 ratio that matches the 20/40 default split. Always
// rounds named UP when the total isn't divisible by 3 (a single-
// slot named pool is more painful than a single-slot anon pool —
// the model uses named for "team roles" that need to exist).
func splitOneToTwo(total int) (int, int) {
	if total <= 0 {
		return 0, 0 // unlimited propagates through
	}
	named := (total + 2) / 3 // ceil(total/3)
	anon := total - named
	return named, anon
}

// parseEnvInt returns a pointer to the int value of env var `name`,
// or nil when the var is unset / non-numeric. Pointer-vs-int because
// 0 is a meaningful intentional value ("disable cap"); we need to
// distinguish "set to 0" from "not set."
func parseEnvInt(name string) *int {
	raw := os.Getenv(name)
	if raw == "" {
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil
	}
	return &n
}
