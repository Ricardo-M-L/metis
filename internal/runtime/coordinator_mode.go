package runtime

// coordinator_mode.go — CoordinatorMode (Phase G.8, 2026-05-12).
//
// When the user launches metis with --coordinator (or
// METIS_COORDINATOR_MODE=1), the main loop's role flips from
// "individual assistant" to "team lead": it should plan work, dispatch
// sub-agents (Agent / Fork / SendMessage), track their work through
// the structured Task* tools, and coordinate their results rather
// than do hands-on edits itself. Two mechanisms wire
// this:
//
//  1. CoordinatorOverlay() returns a SystemPromptSection that's
//     inserted into the assembled system prompt explaining the
//     team-lead expectations. Caller (cmd/metis bootstrap) appends it
//     to AssembleOptions.Overlays.
//
//  2. CoordinatorToolFilter restricts the tool registry to
//     orchestration-relevant tools (Agent, Fork, SendMessage,
//     MessageTeammate, SubAgent* readers, plus Read/Grep/Glob/LS for
//     read-only context-gathering). Direct edit tools (Edit, Write,
//     Bash mutating) are excluded so the coordinator can't quietly
//     bypass its own dispatch model.
//
// Both are no-ops when IsCoordinatorMode() is false — the build is
// always coordinator-mode aware but only the env flag activates it.
//
// Mirrors claude-code's coordinator/lead-agent pattern. The bundled
// `coordinator` agent profile (G.7) is the system-prompt source for
// the body Text — kept in markdown so it's editable without
// recompiling.

import (
	"os"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/tools"
)

// CoordinatorEnvVar is the env-var name that enables coordinator
// mode without a CLI flag. Useful for `METIS_COORDINATOR_MODE=1
// metis chat` and for tests.
const CoordinatorEnvVar = "METIS_COORDINATOR_MODE"

// CoordinatorExtraToolsEnvVar adds extra tool names to the white-
// list. Comma-separated. Recovery hatch for users who hit the
// "coordinator filter too strict" trap — they can opt selected
// tools back in without a code change. Empty = no extras.
const CoordinatorExtraToolsEnvVar = "METIS_COORDINATOR_EXTRA_TOOLS"

// coordinatorModeFlag is the runtime-controlled toggle. cmd/metis
// flips this via SetCoordinatorMode when --coordinator is parsed so
// IsCoordinatorMode() reflects both env and flag uniformly.
var coordinatorModeFlag bool

// SetCoordinatorMode enables/disables coordinator mode programmatic-
// ally. cmd/metis calls this from cli parsing; tests use it to
// activate the mode without env-var fiddling. Once enabled the
// process stays in coordinator mode for its lifetime — there's no
// supported mid-session toggle (would require system prompt rebuild
// + tool registry swap, neither of which is hot-pluggable today).
func SetCoordinatorMode(on bool) { coordinatorModeFlag = on }

// IsCoordinatorMode reports whether the active runtime should treat
// the main loop as a team lead. True when either the CLI flag or
// the env var asks for it. Order doesn't matter — first-set wins
// and both routes are equivalent.
func IsCoordinatorMode() bool {
	if coordinatorModeFlag {
		return true
	}
	return strings.EqualFold(os.Getenv(CoordinatorEnvVar), "1") ||
		strings.EqualFold(os.Getenv(CoordinatorEnvVar), "true")
}

// CoordinatorOverlay returns the system-prompt overlay describing
// the team-lead role. Inserted between `base` and the static-per-
// project sections by the caller. Loads its body from the bundled
// `coordinator` profile (G.7) so the text is co-located with the
// other agent profiles and easy to edit.
//
// Returns an empty SystemPromptSection (Name=="") when coordinator
// mode isn't active or the bundled profile failed to load — caller
// can detect with `s.Name == ""` and skip appending.
func CoordinatorOverlay() SystemPromptSection {
	if !IsCoordinatorMode() {
		return SystemPromptSection{}
	}
	prof, err := LoadBuiltinProfile("coordinator")
	if err != nil || prof == nil || prof.SystemPrompt == "" {
		// Defensive fallback so the mode still does SOMETHING useful
		// even on a busted embed. The tool-filter side will still
		// apply, so the user gets the right tool palette.
		return SystemPromptSection{
			Name:  "coordinator",
			Body:  defaultCoordinatorOverlay,
			Cache: true,
		}
	}
	return SystemPromptSection{
		Name:  "coordinator",
		Body:  prof.SystemPrompt,
		Cache: true,
	}
}

// defaultCoordinatorOverlay is the fallback body used when the
// bundled coordinator.md is missing or unparseable. Intentionally
// shorter than the full markdown — just enough to set up the
// orchestration mindset.
const defaultCoordinatorOverlay = `You are the team lead in a multi-agent workflow.

Your job is to PLAN and DISPATCH work to sub-agents (via the Agent
and Fork tools), then SYNTHESIZE their results — not to do the
hands-on edits yourself. Use Read/Grep/Glob only to gather enough
context to delegate well. Use TaskCreate / TaskUpdate to assign and
track structured work, TaskGet / TaskList to inspect it, TaskOutput
to retain progress and results, and TaskStop to retire cancelled
work. Use SubAgentList / SubAgentOutput to monitor execution and
MessageTeammate to coordinate.

Hands-on tools (Edit, Write, Bash, TodoWrite) are unavailable in this
mode by design. If a task needs code changes, spawn a sub-agent and
hand it the focused task. Quality of the lead's plan + delegation
determines the team's output — not how much code the lead writes.`

// coordinatorAllowedTools is the static whitelist of tool names the
// coordinator may invoke. Anything not in this set is filtered out
// of the registry at build time. The list is curated for the
// "plan, dispatch, sync" workflow:
//
//   - Orchestration: Agent, Fork, SendMessage, MessageTeammate,
//     ScheduleWakeup
//   - Structured work: TaskCreate, TaskGet, TaskList, TaskUpdate,
//     TaskOutput, TaskStop
//   - Sub-agent monitoring: SubAgentList, SubAgentOutput, SubAgentStop
//   - Read-only context-gathering: Read, Grep, Glob, LS
//   - Diagnostics: MetisInfo (so /coordinator status can introspect)
//   - Web reference: WebFetch, WebSearch (lookup docs while planning)
//   - Memory: Memory (so the coordinator can capture team-lead notes)
//
// Notably EXCLUDED: Edit, Write, NotebookEdit, Bash, TodoWrite (todo
// tracking is per-agent, not per-team), Skill (skill invocation is a
// teammate-level concern). Users who legitimately need a tool back
// can add it via METIS_COORDINATOR_EXTRA_TOOLS.
var coordinatorAllowedTools = map[string]struct{}{
	"Agent":           {},
	"Fork":            {},
	"SendMessage":     {},
	"MessageTeammate": {},
	"ScheduleWakeup":  {},
	"TaskCreate":      {},
	"TaskGet":         {},
	"TaskList":        {},
	"TaskUpdate":      {},
	"TaskOutput":      {},
	"TaskStop":        {},
	"SubAgentList":    {},
	"SubAgentOutput":  {},
	"SubAgentStop":    {},
	"Read":            {},
	"Grep":            {},
	"Glob":            {},
	"LS":              {},
	"MetisInfo":       {},
	"WebFetch":        {},
	"WebSearch":       {},
	"Memory":          {},
}

// CoordinatorToolFilter returns the set of tool names that should
// survive the coordinator's restricted registry, given the full set
// passed in. No-op (returns input unchanged) when coordinator mode
// isn't active.
//
// Honors METIS_COORDINATOR_EXTRA_TOOLS so a user who legitimately
// needs to enable, say, TodoWrite for cross-team task tracking can
// opt back in without a recompile.
func CoordinatorToolFilter(all []string) []string {
	if !IsCoordinatorMode() {
		return all
	}
	extras := coordinatorExtraTools()
	out := make([]string, 0, len(all))
	for _, n := range all {
		if _, ok := coordinatorAllowedTools[n]; ok {
			out = append(out, n)
			continue
		}
		if _, ok := extras[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

func coordinatorExtraTools() map[string]struct{} {
	extras := make(map[string]struct{})
	for _, raw := range strings.Split(os.Getenv(CoordinatorExtraToolsEnvVar), ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		// The general visibility grammar accepts broad MCP server prefixes,
		// but this recovery hatch promises explicit tool names. Require a
		// complete mcp__server__tool name so one opt-in cannot silently expose
		// a server's future mutation tools.
		if strings.HasPrefix(name, "mcp__") &&
			(name == "mcp__" || name == "mcp__*" || !strings.Contains(name[len("mcp__"):], "__")) {
			continue
		}
		extras[name] = struct{}{}
	}
	return extras
}

// FilterRegistryInPlace mutates the given registry to drop tools that
// CoordinatorToolFilter excludes. Convenience for callers who hold a
// *tools.Registry rather than a name slice. No-op when coordinator
// mode isn't active.
//
// The visibility layer is durable: later Skill replacement, plugin
// registration, MCP reconnects, and IDE bridges pass through the same
// allowlist instead of escaping a one-time startup snapshot.
func FilterRegistryInPlace(reg *tools.Registry) {
	if !IsCoordinatorMode() || reg == nil {
		return
	}
	extras := coordinatorExtraTools()
	allow := make([]string, 0, len(coordinatorAllowedTools)+len(extras))
	for name := range coordinatorAllowedTools {
		allow = append(allow, name)
	}
	for name := range extras {
		allow = append(allow, name)
	}
	tools.ApplyToolVisibility(reg, allow, nil)
}
