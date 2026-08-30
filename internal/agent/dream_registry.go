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

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// userSkillsDirDefault returns the canonical user-skills directory.
// Routes through config.SkillsDir() so SkillSynth (writer) and the
// curator (archiver) honor METIS_HOME and agree with the
// `metis skills curator` CLI — previously this used os.UserHomeDir
// directly and silently ignored METIS_HOME, so an isolated run touched
// the developer's real ~/.metis/skills.
func userSkillsDirDefault() string {
	return config.SkillsDir()
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
func toolSpecsFromRegistry(reg *tools.Registry, shortDesc bool, contextWindow int, preserve map[string]bool) []llm.ToolSpec {
	all := reg.ModelEntriesForCache()
	out := make([]llm.ToolSpec, 0, len(all))
	deferred := make(map[string]bool)
	for _, entry := range all {
		t := entry.Tool
		out = append(out, llm.ToolSpec{
			Name:        t.Name(),
			Description: descriptionForTool(t, shortDesc),
			InputSchema: t.InputSchema(),
			Exposure:    string(entry.Exposure),
		})
		if entry.Exposure == tools.ToolExposureDeferred {
			deferred[t.Name()] = true
		}
	}
	mode, percentage := parseEnableToolSearch(os.Getenv("ENABLE_TOOL_SEARCH"))
	switch mode {
	case LazyModeAlways:
		return stripAndAppendToolSearchWithExposure(out, deferred, preserve)
	case LazyModeAuto:
		if contextWindow > 0 {
			return applyLazySchemaByTokensWithExposure(out, contextWindow, percentage, deferred, preserve)
		}
	}
	return out
}

func discoveredDeferredFromSpecs(specs []llm.ToolSpec) map[string]bool {
	var out map[string]bool
	for _, spec := range specs {
		if spec.Name == "ToolSearch" || spec.Exposure != string(tools.ToolExposureDeferred) || isLazyPlaceholderSpec(spec) {
			continue
		}
		if out == nil {
			out = make(map[string]bool)
		}
		out[spec.Name] = true
	}
	return out
}
