package agent

// dream_registry.go — builds a tools.Registry tailored to the
// dreaming subagent. The main agent's registry never gets SkillSynth,
// so a regular user-turn can't see it in its tool list, can't ask
// the model to call it, and can't accidentally write skill files.
//
// Pattern: snapshot the parent's tools (so the dream fork inherits
// Read / Grep / Write / Memory etc.), then append a single SkillSynth
// configured to write into the user-layer skills dir. Phase B
// (2026-05-16) — partner of internal/agent/skills/synth.go.

import (
	"os"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// userSkillsDirDefault returns the canonical user-skills directory.
// Empty when UserHomeDir fails — callers should treat empty as
// "SkillSynth unavailable" and skip the wiring.
func userSkillsDirDefault() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".metis", "skills")
}

// buildDreamRegistry returns a fresh *tools.Registry that holds every
// tool from parent plus a SkillSynth bound to skillsDir. If skillsDir
// is empty or the parent is nil, returns parent unchanged so callers
// don't have to special-case wiring failures.
//
// loader is the running skills.Loader (used by Synth.Invalidate so a
// freshly-created skill becomes visible on the NEXT user turn without
// requiring a process restart). Pass nil to skip invalidation —
// safe but means the new skill won't show up in /skills list until
// next launch.
func buildDreamRegistry(parent *tools.Registry, skillsDir string, loader *skills.Loader) *tools.Registry {
	if parent == nil {
		return nil
	}
	if skillsDir == "" {
		return parent
	}
	synth := skills.NewSynth(skillsDir, loader)
	r := tools.NewRegistry()
	for _, t := range parent.All() {
		r.Register(t)
	}
	r.Register(skills.NewSkillSynthTool(synth))
	return r
}

// toolSpecsFromRegistry rebuilds the []llm.ToolSpec the LLM sees in
// its schema list, sourced from reg. Mirrors Loop.toolSpecs() but
// scoped to an arbitrary registry — used by the dreaming fork to
// surface SkillSynth in the request schema (without this override
// the fork inherits the parent's specs, which lack SkillSynth, and
// the model never knows the tool exists).
func toolSpecsFromRegistry(reg *tools.Registry, shortDesc bool) []llm.ToolSpec {
	all := reg.SortedForCache()
	out := make([]llm.ToolSpec, 0, len(all))
	for _, t := range all {
		out = append(out, llm.ToolSpec{
			Name:        t.Name(),
			Description: descriptionForTool(t, shortDesc),
			InputSchema: t.InputSchema(),
		})
	}
	return out
}
