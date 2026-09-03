package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	bashbuiltin "github.com/Ricardo-M-L/metis/internal/tools/builtin/bash"
	worktreepkg "github.com/Ricardo-M-L/metis/internal/worktree"
)

// anonAgentID returns the AgentID string we stamp on each Roster
// entry. Format `agt-<8hex>` keeps it short for /agents list output.
// Used in place of session-local IDs until G.4 (sub-agent resume)
// lands persistent identity.
func anonAgentID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "agt-" + hex.EncodeToString(b[:])
}

// validateTeammateName enforces the G.3 naming rules:
//   - Empty string is fine (caller wants anonymous, Roster auto-assigns).
//   - Must start with a letter so it can't be confused with the
//     `_anon-<hex>` namespace the Roster reserves for anonymous spawns.
//   - Only [a-zA-Z0-9._-] allowed — what slugSegmentRE accepts in the
//     worktree package, kept in lockstep so a teammate name can
//     double as a worktree slug if a future caller wants that.
//   - Max 32 chars — short enough to fit in /agents list output
//     comfortably; claude-code's roster picks a similar bound.
func validateTeammateName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > 32 {
		return fmt.Errorf("teammate name %q exceeds 32 chars", name)
	}
	first := name[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')) {
		return fmt.Errorf("teammate name %q must start with a letter (anonymous slot is reserved for `_anon-<hex>`)", name)
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		ok := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '-'
		if !ok {
			return fmt.Errorf("teammate name %q contains invalid char %q (allowed: letters, digits, . _ -)", name, c)
		}
	}
	return nil
}

// agentDepthKey carries the nested-agent count through context so a
// sub-agent that itself spawns sub-agents can be capped before runaway
// recursion runs the user's bill into the floor.
type agentDepthKey struct{}

// AgentProfileSpec is what an Agent.profileLoader returns. Decoupled
// from internal/runtime.AgentProfile to avoid the import cycle
// (runtime imports builtin, so builtin cannot import runtime). The
// runtime layer wires a thin adapter that translates its full profile
// struct into this shape. Only the fields the Agent tool consumes
// per-invocation are surfaced here; permission/effort/etc. continue
// to be set at boot via the --agent flag.
type AgentProfileSpec struct {
	Name            string
	SystemPrompt    string
	InitialPrompt   string
	Tools           []string // allowlist; nil = inherit
	DisallowedTools []string // blocklist
}

// AgentProfileLoader resolves a subagent_type slug to a profile spec.
// Returning (nil, nil) means "no profile found by this name" — Execute
// surfaces that as IsError so the model can recover (typo, missing
// install). Errors are reserved for parse/IO failures.
type AgentProfileLoader func(name string) (*AgentProfileSpec, error)

// AgentPromptBuildContext is the runtime state needed to assemble the base
// prompt for a cold Agent invocation. Registry is the final child registry,
// after profile and invocation allow/deny filters and child-gate wrapping have
// all been applied.
type AgentPromptBuildContext struct {
	Registry         *tools.Registry
	Provider         llm.Provider
	ProviderName     string
	Model            string
	WorkingDirectory string
}

// AgentMinimalPromptBuilder lets the runtime package assemble a minimal
// sub-agent prompt without creating an import cycle back from builtin to
// runtime. The builder is called at Execute time with the final child registry,
// not when the parent Agent tool is first registered.
type AgentMinimalPromptBuilder func(AgentPromptBuildContext) string

// AgentRuntimePromptState carries the runtime-owned inputs used by the prompt
// builder. It is accepted as an optional argument by the rebind helpers so old
// embedders keep source compatibility while the CLI/Desktop paths can publish
// their exact configured provider name and active workspace.
type AgentRuntimePromptState struct {
	ProviderName         string
	WorkingDirectory     string
	MinimalPromptBuilder AgentMinimalPromptBuilder
}

// bundledProfileSlugs is the back-compat fallback set: when the
// model passes `name` (no `subagent_type`) and the name matches one of
// these, we treat it as if subagent_type was set. Kept in sync with
// internal/runtime/builtin_profiles/*.md.
var bundledProfileSlugs = map[string]struct{}{
	"coordinator":  {},
	"creator":      {},
	"explore":      {},
	"general":      {},
	"go-reviewer":  {},
	"mcp-debugger": {},
	"plan":         {},
	"verify":       {},
}

// defaultMaxAgentDepth is the fallback when neither the Agent struct's
// per-instance MaxDepth nor config.Agents.MaxAgentDepth was set.
//
// 2026-05-16: lowered 3 → 1 to align with claude-code's architectural
// constraint that sub-agents must not spawn further sub-agents
// (Subagent 范式 hard constraint, "子智能体不能再生成其他子智能体").
// Users who want recursive task decomposition (main → plan → explore
// → 子探索, depth ≥ 2) raise `[agents].max_agent_depth` in
// ~/.metis/config.toml — the per-instance Agent.MaxDepth override
// still works for embedders.
//
// Why default 1 and not 0: depth 0 would mean "no sub-agents at all";
// 1 means "the main agent can spawn workers, but those workers stay
// flat" — the CC posture.
const defaultMaxAgentDepth = 1

// Agent is a tool that runs a focused subtask in a fresh agent loop.
// Inspired by Claude Code's LocalAgentTask: spawn an isolated message
// history, execute it to completion, and return the assistant's final text
// to the parent agent as a single tool_result.
//
// The sub-agent shares the parent's provider, tool registry, and permission
// gate. It cannot satisfy interactive permission prompts on its own, so when
// the gate returns Ask the call is denied.
//
// Roster + defaultTimeout were added 2026-05-12 (Phase G.0): the
// Roster caps concurrent sub-agents so a model can't fork-bomb the
// API, and defaultTimeout bounds wall-clock duration when the
// `timeout_seconds` input arg is missing.
//
// jobsPool (added 2026-05-12, Phase G.1) hosts background sub-agents
// when `run_in_background:true`. The Tool spawns the sub-loop in a
// goroutine, registers a Job with the pool, and returns a fast
// handshake `job_id=...` tool_result; the model polls via the
// SubAgentOutput / SubAgentList / SubAgentStop reader tools (mirrors
// claude-code's TaskOutput / TaskList / TaskStop). When jobsPool is
// nil, `run_in_background` requests gracefully fall back to the
// foreground path so existing tests + minimal embeddings still work.
type Agent struct {
	tools.BaseTool
	gate          *permission.Gate
	provider      llm.Provider
	registry      *tools.Registry
	roster        *agent.Roster
	jobsPool      *jobs.Registry // optional; when set, run_in_background works
	model         string
	system        string
	minimalSystem string // optional; preferred for sub-agent loops to save tokens
	// minimalPromptBuilder reassembles the minimal base against the final child
	// registry at invocation time. The adjacent provider/workdir fields are
	// runtime labels rather than values inferred from process-global state.
	minimalPromptBuilder AgentMinimalPromptBuilder
	promptProviderName   string
	promptWorkDir        string
	defaultTimeout       time.Duration
	// sessionDir is the on-disk directory where sub-agent transcripts
	// are persisted for `/agents resume` + the `resume_from` schema
	// field (G.4, 2026-05-12). Empty = persistence disabled — used by
	// tests and the `metis tools` informational listing path.
	sessionDir string
	// parentSessionID stamps the SubAgentOf field on each sub-agent
	// transcript header so `metis sessions list` can group sub-agents
	// under their spawner. Empty = stand-alone.
	parentSessionID string

	// MaxDepth overrides the default nesting cap. 0 → use
	// defaultMaxAgentDepth. Set from config.Agents.MaxAgentDepth at
	// runtime construction. Exposed as a field (not a const) so
	// 2026-05-14+ users can raise the cap via toml without recompiling
	// — the original 3 was hand-picked early on and turns out to be
	// tight for legitimate "main → plan → explore → verify" chains.
	MaxDepth int

	// profileLoader resolves `subagent_type` at invocation time.
	// nil → schema field is accepted but produces an IsError with a
	// hint that profile lookup wasn't wired. Set via WithProfileLoader
	// from the runtime layer (Q1 / 2026-05-15 — separates the
	// "team identity" role from the "which profile to apply" role
	// that name was silently overloading.)
	profileLoader AgentProfileLoader
	// profileNames supplies the currently available profile slugs for the
	// subagent_type JSON Schema enum. It is kept separate from profileLoader so
	// schema generation never has to probe profiles one-by-one. nil preserves
	// compatibility for headless embedders that do not expose a catalog.
	profileNames func() []string
}

// effectiveMaxDepth returns the cap to enforce — instance override
// when set, else the package default.
func (a Agent) effectiveMaxDepth() int {
	if a.MaxDepth > 0 {
		return a.MaxDepth
	}
	return defaultMaxAgentDepth
}

// minimalPromptFor returns the per-invocation base prompt. The preassembled
// minimal/full strings remain the compatibility fallback for embedders that do
// not wire a runtime builder.
func (a Agent) minimalPromptFor(reg *tools.Registry, workDir string) string {
	base := a.system
	if a.minimalSystem != "" {
		base = a.minimalSystem
	}
	if a.minimalPromptBuilder == nil {
		return base
	}
	if workDir == "" {
		workDir = a.promptWorkDir
	}
	providerName := a.promptProviderName
	if providerName == "" && a.provider != nil {
		providerName = a.provider.Name()
	}
	built := a.minimalPromptBuilder(AgentPromptBuildContext{
		Registry:         reg,
		Provider:         a.provider,
		ProviderName:     providerName,
		Model:            a.model,
		WorkingDirectory: workDir,
	})
	if strings.TrimSpace(built) == "" {
		return base
	}
	return built
}

// NewAgent constructs the Agent tool. Caller wires it into the registry
// after builtin.Register so the runtime's provider/model are available.
//
// `system` is the full assembled prompt the parent loop runs with;
// kept as a fallback for callers that don't compute a minimal variant.
// New code should use NewAgentWithMinimal so sub-agents skip the
// parent's <project_context> + ~/.metis/system.md addendum and save
// the per-sub-agent tokens those sections would have eaten.
//
// Roster + defaultTimeout get their zero values; production callers
// should use NewAgentWithMinimal or set them via the package-level
// AttachRoster helper after construction.
func NewAgent(gate *permission.Gate, prov llm.Provider, reg *tools.Registry, model, system string) Agent {
	return Agent{gate: gate, provider: prov, registry: reg, model: model, system: system}
}

// NewAgentWithMinimal is the option-bearing variant. minimalSystem is
// what sub-agents see (mirrors openclaw's "minimal mode" sub-agent
// prompt). When empty, sub-agents fall back to `system`.
func NewAgentWithMinimal(gate *permission.Gate, prov llm.Provider, reg *tools.Registry, model, system, minimalSystem string) Agent {
	return Agent{gate: gate, provider: prov, registry: reg, model: model, system: system, minimalSystem: minimalSystem}
}

// WithRoster wires a Roster into the Agent tool and returns a new
// value. Builder-style so existing callers that ignore the cap don't
// need their signatures changed; runtime/tools.go uses it after
// constructing the shared Roster singleton.
func (a Agent) WithRoster(r *agent.Roster) Agent {
	a.roster = r
	return a
}

// WithDefaultTimeout sets the wall-clock fallback when the
// `timeout_seconds` schema field is not provided. Zero leaves the
// timeout disabled (only the agent loop's max_iter caps execution).
func (a Agent) WithDefaultTimeout(d time.Duration) Agent {
	a.defaultTimeout = d
	return a
}

// WithJobsPool plugs in the jobs.Registry used to host background
// sub-agents when `run_in_background:true`. Without a pool the
// schema field is still accepted (claude-code parity) but it falls
// back to the foreground path so tests + minimal embeddings stay
// functional.
func (a Agent) WithJobsPool(p *jobs.Registry) Agent {
	a.jobsPool = p
	return a
}

// WithSessionPersistence wires the on-disk transcript directory +
// parent session id used by sub-agent resume (G.4). Sub-agents
// without persistence (empty sessionDir) run in-memory only; the
// `resume_from` schema field returns an IsError on those.
//
// The runtime sets these automatically in BuildToolRegistry from
// cfg.Session.Dir and the live session ID; tests pass "" for both
// to keep their isolation against the user's real ~/.metis/.
func (a Agent) WithSessionPersistence(sessionDir, parentID string) Agent {
	a.sessionDir = sessionDir
	a.parentSessionID = parentID
	return a
}

// WithProfileLoader wires the function used to resolve the
// `subagent_type` schema field. Builder-style so headless callers
// (tests, `metis tools` listing) can skip it — passing
// `subagent_type` without a loader wired produces a clear IsError
// rather than silently using the parent's system prompt.
func (a Agent) WithProfileLoader(loader AgentProfileLoader) Agent {
	a.profileLoader = loader
	return a
}

// WithProfileNames wires the catalog published as subagent_type's JSON Schema
// enum. The callback is evaluated whenever InputSchema is requested so newly
// added project/user profiles become visible without rebuilding the registry.
func (a Agent) WithProfileNames(names func() []string) Agent {
	a.profileNames = names
	return a
}

func (Agent) Name() string { return "Agent" }

// ShortDescription — see Bash.ShortDescription for the rationale.
// Agents calling Agents (recursive fork) is rare and discouraged, so
// the short form omits naming/isolation detail in favor of the
// when-vs-when-NOT heuristic.
func (Agent) ShortDescription() string {
	return "Spawn a COLD sub-agent (fresh history, same tools) for self-contained work — code surveys, multi-file scouting, comparative analysis. Pass `subagent_type` to pick a profile (explore/plan/verify/...). For warm spawns that need this conversation's context, use Fork instead. Don't use for single-Grep lookups; spawn has setup overhead."
}

func (Agent) Description() string {
	return `Spawn a COLD sub-agent — a fresh agent loop with its own message history (no shared context with the parent), the same tool set, and a fresh permission gate cloned from yours. Returns a single final text answer. Use this for self-contained, isolated work. (For sub-tasks that need the parent's full conversation history — "based on everything we've discussed, draft X" — use Fork instead, which inherits the parent's context.)

Use Agent for:
  - Deep codebase surveys: "find every place we instantiate a Logger and explain the constructor patterns" — the sub-agent can fan out 10+ Grep/Read calls without bloating your context.
  - Multi-file refactor scouting: "list every Go file that imports the old package path" before you start editing.
  - Comparative analysis: "diff how the auth flow works in package A vs package B."
  - Independent verification: "run the test suite and report which tests failed and the smallest reproducing change."

PARALLEL FAN-OUT (important — this is HOW you scale, not WHEN to ask):
When you have N independent sub-problems, emit N Agent tool_uses IN
THE SAME ASSISTANT TURN. metis's dispatcher launches foreground
calls concurrently and starts run_in_background:true calls as
background jobs that return immediately. Sweet spot is 3–8 parallel
agents; cap is 20 named + 40 anonymous. Example shapes:
  - Surveying 5 libraries → 5 explore agents in one turn, NOT 5
    sequential turns.
  - Implementing 4 independent file clusters → 4 general agents in
    one turn (after a plan agent returned the cluster list).
  - Comparing 3 approaches → 3 explore agents in one turn, then you
    synthesize from their 3 returns.
Do NOT fan out when sub-tasks share state (output of A feeds into
B) or when each target is <2 tool calls (inline is cheaper).
There is no special "spawn_team" tool — multiple Agent tool_use
blocks in one response IS the fan-out mechanism.

DECOMPOSITION BOUNDS (5–30 units):
When decomposing a large task into parallel sub-agents, aim for the
**5–30 independent units** range. Below 5 the spawn/synthesis
overhead exceeds the cost of doing it inline; above 30 the
coordinator can't actually supervise that many — output synthesis
becomes the bottleneck. Mirrors claude-code's batch.ts. Not a hard
runtime cap (no rejection at the (N+1)th call), but work outside
the range usually has a problem worth re-thinking.

Do NOT use Agent for:
  - Lookups you can do in one or two tool calls: a single Grep, a single Read — just do it inline. Forking has overhead (new context window, new system prompt) that costs more than the search.
  - Conversational tasks: explaining something to the user, formatting an answer, deciding what to do next. The model is you; don't fork to think.
  - Tightly-coupled multi-step work where each step depends on the previous one. Sub-agents can't ask the parent questions; if the work needs back-and-forth, do it inline.
  - Tasks that depend on the conversation so far — use Fork (warm spawn). Agent is cold.

Briefing the sub-agent matters more than briefing yourself:
  - State the goal in one sentence, then provide what you already know and what you've ruled out. The sub-agent starts cold — no memory of this conversation.
  - For lookups, hand over the exact pattern or path. For investigations, hand over the question (prescribed steps become dead weight if the premise is wrong).
  - If you need a short response, say so: "report in under 200 words." Without a cap, sub-agents tend to over-explain.
  - Don't ask the sub-agent to make load-bearing decisions for you. Have it gather evidence; you synthesize.
  - Never delegate understanding. Don't write "based on your findings, fix the bug" — that pushes synthesis onto the sub-agent. Instead, give it a narrow research question, get the answer back, then YOU decide and act.

## Examples

<example>
user: What's left on this branch before we can ship?
assistant: Agent(subagent_type="explore", prompt="Audit what's left before this branch can ship. Check: uncommitted changes, commits ahead of main, whether tests exist, whether CI files changed. Report a punch list — done vs. missing. Under 200 words.")
<reasoning>
A survey question that would dump git output into context. Delegating keeps the parent's window clean; the explicit "under 200 words" cap stops the sub-agent over-explaining.
</reasoning>
</example>

<example>
user: Refactor the entire auth module to use the new session API.
assistant: First spawns a plan agent: Agent(subagent_type="plan", prompt="Read internal/auth/*.go. List every site that calls the old SessionStore.Get. Group by file. Output: ordered list of files to edit + which call to replace in each. Do NOT edit anything.")
*Receives the plan back.*
Then spawns 4 implementation agents in PARALLEL (one assistant turn, 4 Agent blocks): Agent(subagent_type="general", prompt="In file X, replace ...")
<reasoning>
Plan first (cold scout), then fan out — the 4 impl agents are independent so they run in one turn. Never let one large agent do "plan AND implement" — context bloat + no chance to course-correct between phases.
</reasoning>
</example>

` + "`name`" + ` vs ` + "`subagent_type`" + ` (Q1, 2026-05-15 — the two roles are now separate):
  - ` + "`subagent_type`" + ` selects the PROFILE/ROLE — its system prompt, default tool allowlist, default model. Bundled: explore, plan, creator, verify, general, go-reviewer, mcp-debugger, coordinator, teammate. User-defined profiles in ~/.metis/agents/<slug>.md or ./.metis/agents/<slug>.md take precedence over bundled. Omit to inherit the parent's prompt.
  - ` + "`name`" + ` is the TEAM IDENTITY only — what /agents and MessageTeammate use to address this worker ("alice", "verifier"). Same-name collisions auto-suffix (alice → alice-2 → alice-3 → ...).
  - Back-compat: if you pass ` + "`name=\"explore\"`" + ` without ` + "`subagent_type`" + `, it's still treated as ` + "`subagent_type=\"explore\"`" + `. Explicit subagent_type is preferred — name should be a label like "alice", not a role like "explore".

Other knobs:
  - ` + "`isolation: \"worktree\"`" + ` gives the sub-agent its own git worktree under ~/.metis/worktrees/ — useful for risky experiments that shouldn't touch the parent's checkout. Auto-cleaned on exit. Refused if you're already inside a worktree (no nesting), and unavailable while the parent is in Plan because setup changes git metadata.
  - ` + "`cwd`" + ` runs the sub-agent in a specific directory (mutually exclusive with ` + "`isolation`" + `).
  - ` + "`run_in_background: true`" + ` → returns job_id immediately, poll via SubAgentOutput, terminate via SubAgentStop.
  - ` + "`permission_mode`" + ` overrides the gate just for this sub-agent (e.g. constrain a worker to "plan" while the parent stays in "default"). A Plan parent may only inherit Plan or explicitly request "plan"; approving a plan starts a fresh implementation turn instead of upgrading an already-running child. A fullAccess parent must omit this field (inherit fullAccess) or explicitly keep fullAccess: its disabled process sandbox and parent-bound tool instances cannot safely enforce a lower child mode.
  - ` + "`allowed_tools`" + ` / ` + "`disallowed_tools`" + ` narrow the sub-agent's tool view; combine with the profile's filters as INTERSECTION (allow) + UNION (deny).`
}
func (a Agent) InputSchema() map[string]any {
	schema := map[string]any{
		"type":     "object",
		"required": []string{"prompt"},
		"properties": map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "the focused task for the sub-agent",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "Optional 3-5 word human-readable summary of the task (e.g. \"search auth flow\", \"fix login bug\"). Shown in the TUI's collapsed tool-call header as `Explore (search auth flow)` so the user can see WHAT this sub-agent is doing without expanding the prompt. claude-code-style label. Keep it short — 6 words max.",
			},
			"max_iter": map[string]any{
				"type":        "integer",
				"description": "tool-call budget for the sub-agent (default 100). Bumped from 10 → 100 on 2026-05-21 after image #36 repro: impl-* sub-agents kept hitting the 10-turn cap mid-implementation. claude-code's equivalent (forkSubagent.maxTurns) is 200; metis goes 100 as a middle ground that covers typical large file rewrites without unbounded runaway.",
			},
			"timeout_seconds": map[string]any{
				"type":        "integer",
				"description": "wall-clock budget for the sub-agent. Defaults to config.Agents.DefaultTimeoutSeconds (10 minutes). 0 disables timeout.",
			},
			"run_in_background": map[string]any{
				"type":        "boolean",
				"description": "Set to true to spawn the sub-agent in the background and return a job_id immediately so the parent can continue working. Poll progress via SubAgentOutput / SubAgentList, terminate via SubAgentStop. Mirrors claude-code's AgentTool semantics. Default false (foreground).",
			},
			"isolation": map[string]any{
				"type":        "string",
				"enum":        []string{"worktree"},
				"description": "Spawn this sub-agent in an isolated git worktree under ~/.metis/worktrees/. The sub-agent's tools see the worktree as cwd, so file writes don't touch the parent's checkout. Worktree is auto-cleaned when the sub-agent exits (or when parent ctx cancels). Mutually exclusive with `cwd`. Refuses when parent is already inside a worktree (no nesting) or when the parent is in Plan mode (worktree setup changes git metadata).",
			},
			"cwd": map[string]any{
				"type":        "string",
				"description": "Absolute path to run the sub-agent in. Overrides the parent's working directory for all filesystem and shell operations within this sub-agent. Mutually exclusive with `isolation: \"worktree\"`.",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Optional team identity for this sub-agent. Named teammates show up in /agents list and can be addressed by MessageTeammate({to: name, body: ...}) from other sub-agents. Must start with a letter; only [a-zA-Z0-9._-] allowed; max 32 chars. Same-name collisions auto-suffix (alice → alice-2 → alice-3 ...). NOTE: `name` is the LABEL; use `subagent_type` to pick the profile/role.",
			},
			"subagent_type": map[string]any{
				"type":        "string",
				"description": "Optional profile slug that determines the sub-agent's role + system prompt. Bundled profiles: explore (read-only code search), plan (architect/planning), creator (end-to-end implementation), verify (test runner), general (catch-all), go-reviewer (Go-specific code review), mcp-debugger (MCP issues), coordinator (delegator), teammate (long-running team member with peer-message + shared-task-list coordination — pick this when you spawn multiple named workers that need to talk to each other). User-defined profiles in ~/.metis/agents/<name>.md or ./.metis/agents/<name>.md take precedence over bundled. When omitted, the sub-agent inherits the parent's system prompt. When `name` is set without `subagent_type`, a name matching a known profile slug is treated as the subagent_type for back-compat — explicit `subagent_type` is preferred.",
			},
			"resume_from": map[string]any{
				"type":        "string",
				"description": "Resume a previously-paused sub-agent by its agent_id (e.g. \"agt-d3a91b07\"). The on-disk transcript is replayed and a fresh sub-loop continues from the last turn. The `prompt` field is used as a follow-up turn appended to the recovered history. Use with care: a sub-agent that's still alive somewhere else WILL cause undefined behavior — resume only after the original sub-agent's run has fully ended (SubAgentList shows it gone, or you killed it via SubAgentStop).",
			},
			"permission_mode": map[string]any{
				"type":        "string",
				"enum":        []string{"default", "acceptEdits", "plan", "dontAsk", "bypassPermissions", "fullAccess"},
				"description": "Override the permission mode for this sub-agent's gate using a Claude Code public mode. The parent's gate is unchanged; the sub-agent gets a clone with its own mode + a fresh denial-streak counter. Omit to inherit the parent's current mode. While the parent is in Plan, this must be omitted or set to `plan`; an existing Plan child is never upgraded after plan approval. While the parent is in fullAccess, this must be omitted or set to `fullAccess` because a child cannot restore the disabled process sandbox or replace parent-bound tool instances.",
			},
			"allowed_tools": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional allowlist of tool names the sub-agent may invoke. Combined as INTERSECTION with the agent profile's `tools` filter when both are set, so the sub-agent always sees the strictest result. Empty / omitted = inherit (no per-invocation restriction).",
			},
			"disallowed_tools": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional blocklist applied AFTER the allowed_tools intersection. Combined as UNION with the profile's `disallowed_tools` when both are set, so denying a tool at the call site never gets quietly re-enabled by the profile.",
			},
		},
	}
	if a.profileNames != nil {
		if names := normalizedProfileNames(a.profileNames()); len(names) > 0 {
			properties := schema["properties"].(map[string]any)
			subagentType := properties["subagent_type"].(map[string]any)
			subagentType["enum"] = names
		}
	}
	return schema
}

func normalizedProfileNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			seen[name] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// Concurrency is input-aware (Phase G.1, 2026-05-12): a foreground
// Agent call still serializes Exclusive after safe/queue work; a
// background call declares ConcurrencyBackground so the dispatcher
// kicks it off without gating subsequent tools on its completion.
//
// claude-code does the same thing implicitly via task lifecycle —
// the AgentTool returns immediately for run_in_background and the
// dispatcher proceeds. Doing it via Concurrency() keeps metis's
// dispatch graph honest: parallelism is declared, not implicit.
func (Agent) Concurrency(in map[string]any) tools.Concurrency {
	if b, ok := in["run_in_background"].(bool); ok && b {
		return tools.ConcurrencyBackground
	}
	// Foreground Agent calls emitted in the same assistant message are
	// independent child loops. Classify them as safe so executeBatch can fan
	// them out concurrently; the Agent's shared capacity limiter still bounds
	// total child work and executeBatch preserves result order.
	return tools.ConcurrencySafe
}

func (a Agent) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	parentWasPlan := a.gate != nil && a.gate.Mode() == permission.ModePlan
	if err := validatePlanAgentInput(parentWasPlan, in); err != nil {
		return tools.PermissionDeny, err.Error()
	}
	if a.gate == nil {
		return tools.PermissionDeny, "Agent tool has no permission gate"
	}
	if err := validateAgentPermissionOverride(a.gate.Mode(), in); err != nil {
		return tools.PermissionDeny, err.Error()
	}
	d, src := a.gate.Check(ctx, "Agent", marshalAgentToolInput(in))
	return mapDecision(d), src
}

func (a Agent) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	// Capture the parent's posture at the actual execution boundary. CanUse
	// runs after PreToolUse hooks, but the UI can still change modes before a
	// queued/background call reaches Execute. A call that starts in Plan must
	// stay Plan-scoped for its entire child lifetime even if the parent later
	// leaves Plan.
	parentWasPlan := a.gate != nil && a.gate.Mode() == permission.ModePlan
	prompt, _ := in["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	if a.provider == nil || a.registry == nil || a.gate == nil {
		return nil, errors.New("Agent tool not fully wired (missing provider/registry/gate)")
	}
	depth, _ := ctx.Value(agentDepthKey{}).(int)
	if cap := a.effectiveMaxDepth(); depth >= cap {
		return &tools.Result{
			Output:  fmt.Sprintf("agent nesting limit (%d) exceeded — raise [agents].max_agent_depth in ~/.metis/config.toml if this is legitimate", cap),
			IsError: true,
		}, nil
	}
	if err := validatePlanAgentInput(parentWasPlan, in); err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}
	if err := validateAgentPermissionOverride(a.gate.Mode(), in); err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}
	requestedMode, hasRequestedMode, err := requestedAgentPermissionMode(in)
	if err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}

	// G.2 — resolve per-invocation isolation/cwd BEFORE registering
	// the teammate so we can refuse the spawn without polluting the
	// Roster on bad inputs. Returns the effective cwd to thread
	// through the sub-loop ctx + an optional worktree info we'll
	// clean up on exit.
	isolation, _ := in["isolation"].(string)
	cwdArg, _ := in["cwd"].(string)
	subCwd, worktreeInfo, isoErr := a.resolveIsolation(isolation, cwdArg)
	if isoErr != nil {
		return &tools.Result{Output: isoErr.Error(), IsError: true}, nil
	}
	// resolveIsolation may already have created a worktree. Keep ownership in
	// Execute until the child finalizer is installed; every validation/setup
	// error in between must still remove it.
	worktreeCleanupOwnedByExecute := worktreeInfo != nil
	defer func() {
		if worktreeCleanupOwnedByExecute {
			_ = worktreepkg.Cleanup(worktreeInfo)
		}
	}()

	// G.3 (2026-05-12) — `name` field for named teammates. Validated
	// before any roster work so a bad name fails fast without
	// polluting the cap. The check is intentionally strict (letter
	// prefix + ascii subset) to make names safe to print in /agents
	// list and to avoid colliding with the `_anon-<hex>` namespace
	// the Roster reserves for anonymous spawns.
	nameArg, _ := in["name"].(string)
	if err := validateTeammateName(nameArg); err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}

	// G.4 (2026-05-12) — `resume_from` field for sub-agent resume.
	// Load the on-disk snapshot now so we can fail fast (file not
	// found / corrupted) before consuming a Roster slot. The loaded
	// snapshot's AgentID becomes the new teammate's AgentID so the
	// transcript file is appended to, not overwritten.
	resumeFrom, _ := in["resume_from"].(string)
	var resumedSnapshot *agent.SubAgentSnapshot
	if resumeFrom != "" {
		if a.sessionDir == "" {
			return &tools.Result{
				Output:  "resume_from requires session persistence to be wired; this code path (likely a test or `metis tools` listing) has no session dir.",
				IsError: true,
			}, nil
		}
		// Mi2 (2026-05-18) — refuse if the source agent_id is still
		// LIVE. Two parallel sub-loops sharing the same AgentID would
		// race on the transcript file and produce undefined behavior.
		// Roster.LookupByAgentID scans live first, then recentlyFinished;
		// liveness check separates the two by also reading the live map
		// directly. (recentlyFinished hits are safe — the original
		// runner has exited.)
		if a.roster != nil {
			if existing, ok := a.roster.LookupByAgentID(resumeFrom); ok {
				snap := existing.Snapshot()
				if snap.Status == agent.StatusRunning {
					return &tools.Result{
						Output: fmt.Sprintf(
							"resume_from=%s refused: sub-agent is still RUNNING (name=%s, started=%s). "+
								"Stop it first with SubAgentStop({agent_id: %q}) and wait for it to finish, "+
								"then re-issue the resume.",
							resumeFrom, snap.Name, snap.Started.Format("15:04:05"), resumeFrom,
						),
						IsError: true,
					}, nil
				}
			}
		}
		snap, err := agent.LoadSubAgentSnapshot(a.sessionDir, resumeFrom)
		if err != nil {
			return &tools.Result{
				Output:  fmt.Sprintf("resume_from=%s failed: %v", resumeFrom, err),
				IsError: true,
			}, nil
		}
		resumedSnapshot = snap
	}

	// G.0 cap — refuse before constructing the sub-loop so the API
	// burn stays bounded. The cap reads from the Roster's Capacity
	// (set at runtime construction from config.Agents.MaxConcurrentSubAgents).
	// When no Roster is attached we skip the check, matching the
	// pre-G.0 behavior used by unit tests that construct Agent directly.
	runInBackground, _ := in["run_in_background"].(bool)
	var teammate *agent.Teammate
	// Execute owns roster cleanup from the instant Register succeeds until a
	// child runner has actually started. This includes background setup: profile
	// lookup and the remaining sub-loop construction can still fail after the
	// slot is consumed, and no goroutine exists yet to unregister it. The
	// ownership guard prevents those early returns from leaking a live teammate
	// (and its done channel) forever.
	rosterCleanupOwnedByExecute := false
	defer func() {
		if rosterCleanupOwnedByExecute && a.roster != nil {
			// UnregisterTeammate closes the strict lifecycle join edge. On
			// pre-run setup failures there is no runner finalizer yet, so finish
			// worktree cleanup here before publishing that the teammate is done.
			if worktreeCleanupOwnedByExecute {
				_ = worktreepkg.Cleanup(worktreeInfo)
				worktreeCleanupOwnedByExecute = false
			}
			a.roster.UnregisterTeammate(teammate)
		}
	}()
	if a.roster != nil {
		// G.4 — resumed sub-agents keep their original AgentID so the
		// transcript file path is stable across resume cycles.
		agentID := anonAgentID()
		if resumedSnapshot != nil && resumedSnapshot.Header.ID != "" {
			agentID = resumedSnapshot.Header.ID
		}
		// Resumed teammate likewise inherits its prior name if there
		// was one (so /agents resume alice → "alice" comes back as a
		// teammate, not _anon-...).
		effectiveName := nameArg
		if resumedSnapshot != nil && resumedSnapshot.Header.TeammateName != "" && effectiveName == "" {
			effectiveName = resumedSnapshot.Header.TeammateName
		}
		teammate = &agent.Teammate{
			Name:       effectiveName, // empty → Roster auto-assigns _anon-<hex>
			AgentID:    agentID,
			Background: runInBackground,
		}
		if err := a.roster.Register(teammate); err != nil {
			if errors.Is(err, agent.ErrCapacityExceeded) {
				// Split pools: the error came from whichever pool
				// matches this teammate's kind. Report THAT pool's
				// numbers, not the total — the user can spawn the
				// other kind freely.
				summary := a.roster.Summary()
				var inPool, capPool int
				var kindLabel, kindKnob, envKnob string
				if teammate.Anonymous {
					inPool, capPool = summary.Anonymous, a.roster.CapacityAnon()
					kindLabel, kindKnob, envKnob = "anonymous sub-agent", "max_concurrent_anon", "METIS_MAX_SUBAGENTS_ANON"
				} else {
					inPool, capPool = summary.Named, a.roster.CapacityNamed()
					kindLabel, kindKnob, envKnob = "named teammate", "max_concurrent_named", "METIS_MAX_SUBAGENTS_NAMED"
				}
				return &tools.Result{
					Output: fmt.Sprintf(
						"%s capacity exceeded (%d/%d in flight). Wait for one to finish, raise [agents].%s in ~/.metis/config.toml, or export %s=N for an immediate per-session override.",
						kindLabel, inPool, capPool, kindKnob, envKnob,
					),
					IsError: true,
				}, nil
			}
			if errors.Is(err, agent.ErrNameInUse) {
				return &tools.Result{
					Output: fmt.Sprintf(
						"teammate name %q is already in use by another live sub-agent. Pick a different name or wait for the other to finish.",
						nameArg,
					),
					IsError: true,
				}, nil
			}
			return &tools.Result{Output: err.Error(), IsError: true}, nil
		}
		rosterCleanupOwnedByExecute = true
	}

	// Sub-loop construction is identical in both paths.
	//
	// Default bumped 10 → 100 on 2026-05-21. The 10-turn cap was
	// causing impl-* sub-agents to hit budget mid-implementation
	// without producing any output (session 13a82094 / image #36:
	// "impl-tools-read sub-agent hit budget without output. I'll
	// write all the tool files myself in parallel."). claude-code's
	// forkSubagent runs at 200 turns; metis picks 100 as a middle
	// ground — enough for a full file rewrite, still bounded for
	// runaway protection.
	maxIter := intArg(in, "max_iter", 100)
	// G.13 (2026-05-12) — pick the right sub-agent prompt template.
	// Named teammates get extra peer-messaging guidance; anonymous
	// spawns get the focused-task posture. Both prepend the role
	// preamble to whatever the parent's minimalSystem already had.
	subPromptMode := agent.SubPromptAgent
	teammateName := ""
	if teammate != nil {
		teammateName = teammate.Name
		if !teammate.Anonymous {
			subPromptMode = agent.SubPromptTeammate
		}
	}

	// Q1 (2026-05-15) — resolve `subagent_type` for per-invocation
	// profile selection. Back-compat: if subagent_type is empty but
	// `name` happens to match a bundled profile slug (explore, plan,
	// verify, ...), treat it as the subagent_type so the historical
	// docstring claim ("name auto-selects a profile") still holds for
	// callers that learned it from the old description. Explicit
	// subagent_type is preferred and always wins.
	subagentType, _ := in["subagent_type"].(string)
	subagentType = strings.TrimSpace(subagentType)
	if subagentType == "" {
		if _, isBundled := bundledProfileSlugs[nameArg]; isBundled {
			subagentType = nameArg
		}
	}
	var profile *AgentProfileSpec
	if subagentType != "" {
		if a.profileLoader == nil {
			return &tools.Result{
				Output:  fmt.Sprintf("subagent_type=%q requires profile loading, which isn't wired in this build (likely a headless test path).", subagentType),
				IsError: true,
			}, nil
		}
		p, err := a.profileLoader(subagentType)
		if err != nil {
			return &tools.Result{
				Output:  fmt.Sprintf("subagent_type=%q failed: %v", subagentType, err),
				IsError: true,
			}, nil
		}
		if p == nil {
			return &tools.Result{
				Output:  fmt.Sprintf("subagent_type=%q: no such profile (looked in ./.metis/agents, ~/.metis/agents, and the bundled set: explore, plan, creator, verify, general, go-reviewer, mcp-debugger, coordinator, teammate)", subagentType),
				IsError: true,
			}, nil
		}
		profile = p
	}

	// G.9 (2026-05-12) — give the sub-agent its OWN gate so a
	// child's permission-mode flip doesn't leak back into the parent
	// (and so the sub-agent's denial-streak counter doesn't bleed
	// into the parent's circuit breaker). The clone shares rules +
	// classifier — the snapshot is mode + rules at spawn time.
	// Profile-driven mode comes via the schema field `permission_mode`.
	subGate := a.gate.Clone()
	if parentWasPlan {
		// A Plan child is immutable for its full lifetime. In particular, a
		// background child must not gain write access when the parent approves
		// the plan and changes its own gate to acceptEdits/default/bypass.
		subGate.SetMode(permission.ModePlan)
	} else if hasRequestedMode {
		subGate.SetMode(requestedMode)
	}

	// G.14 (2026-05-12) — per-invocation tool filter. The schema
	// fields `allowed_tools` (allowlist) and `disallowed_tools`
	// (blocklist) narrow the sub-agent's view of the registry
	// without affecting the parent's view. Empty filters = inherit
	// the full parent registry. Filter applies BEFORE the sub-loop
	// is constructed so the sub-agent never sees a tool it
	// shouldn't even try to call.
	//
	// Q1 (2026-05-15) — when subagent_type resolves to a profile with
	// its own tool restrictions, we intersect them with the schema
	// filters so per-invocation NEVER quietly re-enables a tool the
	// profile blocked. Profile allowlist + schema allowlist =
	// intersection (strictest). Profile blocklist + schema blocklist
	// = union.
	allowedTools := stringSliceArg(in, "allowed_tools")
	disallowedTools := stringSliceArg(in, "disallowed_tools")
	if profile != nil {
		allowedTools = intersectAllow(profile.Tools, allowedTools)
		disallowedTools = unionStrings(profile.DisallowedTools, disallowedTools)
	}
	filteredRegistry := a.registry
	if len(allowedTools) > 0 || len(disallowedTools) > 0 {
		filteredRegistry = filterRegistry(a.registry, allowedTools, disallowedTools)
	}
	planLocked := subGate.Mode() == permission.ModePlan
	// Always build a distinct, child-gated registry. Merely cloning Gate is
	// insufficient because the concrete tools in a.registry still capture the
	// parent's Gate pointer. The outer wrapper freezes the child policy; Plan
	// children additionally lose permission-control, nested-agent, and Skill
	// invocation surfaces entirely.
	// A child must not consume the process-wide job notification channel: with
	// multiple agents, whichever loop reads first would steal another loop's
	// completion. Clone the filtered registry, rebind only its existing Bash
	// job tools to a private pool, then wrap them with the immutable child gate.
	// This also preserves profile/per-call tool filters (Rebind never adds a
	// missing BashOutput/BashList/BashKill tool).
	var childJobs *jobs.Registry
	if a.jobsPool != nil {
		filteredRegistry = copyToolRegistry(filteredRegistry)
		childJobs = jobs.NewRegistry("")
		bashbuiltin.RebindJobsRegistry(filteredRegistry, childJobs, subGate)
	}
	subRegistry := agentChildRegistry(filteredRegistry, subGate, planLocked)

	// Assemble the cold Agent base only after every profile/invocation filter
	// and child-gate restriction has produced the registry the child will
	// actually receive. A runtime-supplied builder can therefore keep tool-aware
	// guidance, provider labels, and the workspace env in lockstep with reality.
	promptWorkDir := subCwd
	if promptWorkDir == "" {
		promptWorkDir = agent.CwdFromContext(ctx)
	}
	base := a.minimalPromptFor(subRegistry, promptWorkDir)
	// 2026-05-15 fix — profile body goes into ProfileSystemPrompt,
	// NOT replacing base. Pre-fix the profile fully replaced base,
	// which dropped the parent's <env> section (Working directory,
	// Today's date, Git branch). Sub-agents then had no idea what
	// cwd they were in and either guessed wrong absolute paths
	// (/internal/..., /home/user/code/..., /workspace/...) or used
	// relative paths that the path-strict Read tool rejected. The
	// 200-iter long-Wall test showed 110+ such errors. Keeping base
	// preserves env; ProfileSystemPrompt adds the role-specific
	// guidance after it.
	profileBody := ""
	if profile != nil {
		profileBody = profile.SystemPrompt
	}
	subSystem := agent.BuildSubPrompt(agent.SubPromptInputs{
		Mode:                subPromptMode,
		Base:                base,
		TeammateName:        teammateName,
		ProfileSystemPrompt: profileBody,
	})
	if planLocked {
		subSystem += "\n\n<plan_subagent_boundary>\nYou are a read-only planning and investigation sub-agent. Inspect with the available read-only tools, then return your findings or proposed plan directly to the parent. Do not implement changes and do not attempt to call EnterPlanMode or ExitPlanMode; only the parent can request plan approval and begin a fresh implementation turn.\n</plan_subagent_boundary>"
	}

	sub := agent.NewLoop(a.provider, subRegistry, subGate, agent.NewHookRegistry(), subSystem, maxIter)
	sub.Model = a.model
	sub.RecoverTextToolCalls = true
	if childJobs != nil {
		sub.JobNotify = childJobs.Notify()
		sub.Jobs = childJobs
	}
	// Agent is allowed as read-only delegation during plan mode. Keep the
	// child loop's live PlanMode in sync with its cloned gate so NewLoop's
	// fallback Plan prompt is attached and mutating child calls are returned as
	// denied results instead of the child repeatedly attempting them.
	if subGate != nil && subGate.Mode() == permission.ModePlan {
		sub.SetPlanMode(true)
	}
	// Shared USD budget: the child adds usage to the parent's tracker,
	// so --max-budget-usd caps the whole agent tree, not just the root.
	sub.Budget = agent.BudgetFromContext(ctx)
	// Shared spill dir: the child offloads its own oversized tool
	// results too, instead of flooding its context window.
	sub.SpillDir = agent.SpillDirFromContext(ctx)
	// Sub-agents get the curated short-form tool descriptions: they
	// already inherit a tight tool palette + a profile-supplied system
	// prompt, so the main-loop's full multi-paragraph tool docs are
	// pure noise. Phase C.1 / 2026-05-14.
	sub.ShortToolDescriptions = true
	// G.4 — restore prior conversation history BEFORE appending the
	// new prompt. This way the resumed sub-agent sees the recovered
	// turns followed by the caller's follow-up message, which the
	// model should treat as "given what we already discussed, do X".
	if resumedSnapshot != nil && len(resumedSnapshot.Messages) > 0 {
		sub.Restore(resumedSnapshot.Messages)
	}
	// Q1 (2026-05-15) — profile-supplied initial_prompt gets prepended
	// to the caller's prompt (matches the --agent CLI flag's
	// behavior). Mirrors claude-code's Task tool where the profile's
	// `initial_prompt` block frames the directive.
	effectivePrompt := prompt
	if profile != nil && profile.InitialPrompt != "" {
		effectivePrompt = profile.InitialPrompt + "\n\n" + prompt
	}
	sub.AppendUser(effectivePrompt)
	// G.3 — wire the teammate's Mailbox to the sub-loop's PeerInbox so
	// the agent.Loop drains it at iter boundaries and injects
	// <peer_message> system-reminders. nil Mailbox (no Roster wired)
	// leaves PeerInbox nil — peer messaging silently disabled, which
	// is the right behavior for headless unit tests.
	if teammate != nil {
		sub.PeerInbox = teammate.Mailbox
	}

	// G.4 — open a transcript writer if persistence is wired. Foreground
	// & background paths both append to it via the event-forwarding
	// loop (see persistNewMessages below). Close is deferred in the
	// foreground path; the background goroutine handles its own close.
	//
	// persistedOnDisk tracks how many messages from sub.History() we've
	// already written. The event loop bumps it on every EventTurnEnd
	// so each new turn's tail gets appended exactly once. For fresh
	// sub-agents this starts at 0 (header-only on disk); for resumed
	// sub-agents it starts at len(snapshot.Messages) — the count of
	// historic messages we just rewrote — so the new user prompt
	// (which sub.AppendUser already added to sub.Messages) DOES get
	// flushed when the first turn ends.
	var transcript *agent.SubAgentTranscript
	persistedOnDisk := 0
	if a.sessionDir != "" && teammate != nil {
		hdr := agent.NewSubAgentHeader(
			teammate.AgentID,
			a.model,
			a.parentSessionID,
			teammate.Name,
			subCwd,
			// G.9 — record the sub-agent's effective mode (post
			// permission_mode override), not the parent's, so
			// `/agents resume` rebuilds the same posture.
			string(subGate.Mode()),
		)
		t, err := agent.NewSubAgentTranscript(a.sessionDir, teammate.AgentID, hdr)
		if err == nil {
			transcript = t
			// On resume, rewrite the recovered messages immediately so
			// the file isn't a stub-with-header-only if the sub-agent
			// errors before producing new output.
			if resumedSnapshot != nil {
				for _, m := range resumedSnapshot.Messages {
					_ = transcript.AppendMessage(m)
				}
				persistedOnDisk = len(resumedSnapshot.Messages)
			}
		}
	}
	// Keep transcript ownership in Execute until a foreground/background
	// runner has accepted it. Today no ordinary return exists in the remaining
	// setup block, but this guard also closes the descriptor if a future setup
	// validation or panic is added before the runner finalizer is installed.
	transcriptCleanupOwnedByExecute := transcript != nil
	defer func() {
		if transcriptCleanupOwnedByExecute {
			_ = transcript.Close()
		}
	}()

	// G.0 timeout — wall-clock cap. Caller-provided `timeout_seconds`
	// wins; else config default; 0 disables.
	timeoutSec := intArg(in, "timeout_seconds", 0)
	timeout := time.Duration(timeoutSec) * time.Second
	if timeoutSec == 0 && a.defaultTimeout > 0 {
		timeout = a.defaultTimeout
	}

	// Extract the parent loop's sub-agent notification channel BEFORE
	// building baseCtx. The channel was stamped into ctx by the parent
	// Loop.Run(); we pull it out here so we can pass it directly to
	// executeBackground — and explicitly shadow the key with nil in
	// baseCtx so child sub-agents (depth >= 2) don't accidentally
	// write to the grandparent's notify channel.
	parentNotify := agent.SubAgentNotifyFromContext(ctx)

	// Build the sub-agent ctx with depth + cwd + (optional) timeout.
	// For the background path the goroutine owns the cancel func and
	// keeps the ctx alive past Execute's return — context.WithCancel
	// doesn't leak when the deferred goroutine runs cancel on its
	// own exit. G.2: subCwd stamps the effective working directory
	// onto the ctx so cwd-aware tools (Bash) inherit it.
	// subAgentNotify key is explicitly cleared (nil) so the child
	// loop's tools don't see the grandparent's notify channel.
	baseCtx := agent.WithSubAgentNotify(
		agent.WithCwd(context.WithValue(ctx, agentDepthKey{}, depth+1), subCwd),
		nil,
	)
	// A child loop owns an internal events channel, but it has no human reply
	// consumer. Mark that distinction explicitly: EventOut != nil alone only
	// means the loop can stream progress, not that AskUser can safely block.
	// The marker is scoped to this child context, so the parent's real TUI keeps
	// its ordinary AskUser behavior even when both loops share fullAccess.
	baseCtx = agent.WithUserInteractionUnavailable(
		baseCtx,
		"interactive tool unavailable in sub-agent execution; choose a reasonable default or return the question to the parent as text",
	)
	// Stamp the teammate's roster name so the child's tools (especially
	// MessageTeammate) can read the correct sender identity via
	// AgentNameFromContext instead of falling back to "main".
	baseCtx = agent.WithAgentName(baseCtx, teammateName)
	// Background sub-agents must outlive the parent turn. The TUI tears
	// down each turn's context (cancel) and closes the turn's event
	// channel the moment loop.Run returns; without detaching here, a
	// background goroutine inheriting the turn ctx is instantly killed
	// ("killed: context canceled") the first time its parent turn ends -
	// even while the agent is still mid-tool-call. context.WithoutCancel
	// keeps all context values (depth, cwd, budget, event-out key used by
	// forwardSubAgentEvent) but severs the turn's cancellation chain so
	// the sub-agent is session-scoped, killed only by its own timeout /
	// SubAgentStop (teammate.Cancel) / Roster.CancelAll on exit.
	if runInBackground {
		baseCtx = context.WithoutCancel(baseCtx)
	}
	// Pick ONE cancel context. The old form created WithCancel(baseCtx) then
	// reassigned both vars from WithTimeout(childCtx, timeout) when timeout>0,
	// orphaning the first cancel (lostcancel: a leaked cancelCtx registered on
	// baseCtx until the parent turn ends — one per timed sub-agent spawn).
	var childCtx context.Context
	var cancelRun context.CancelFunc
	var lifecycleCtx context.Context
	var cancelLifecycle context.CancelFunc
	// InterruptBlock must ignore an ordinary first-Ctrl+C inherited through
	// baseCtx, but not an explicit teammate stop, session revoke, or the child's
	// own timeout. Give it a distinct hard-lifecycle root detached from the turn.
	hardBaseCtx := context.WithoutCancel(baseCtx)
	if timeout > 0 {
		childCtx, cancelRun = context.WithTimeout(baseCtx, timeout)
		lifecycleCtx, cancelLifecycle = context.WithTimeout(hardBaseCtx, timeout)
	} else {
		childCtx, cancelRun = context.WithCancel(baseCtx)
		lifecycleCtx, cancelLifecycle = context.WithCancel(hardBaseCtx)
	}
	signalCancel := func() {
		cancelRun()
		cancelLifecycle()
	}
	// InterruptBlock dispatch detaches ordinary turn cancellation. Stamp the
	// child's own cancelable lifetime so SubAgentStop / timeout / roster reset
	// can still stop a foreground Bash process tree immediately.
	childCtx = agent.WithToolLifecycleContext(childCtx, lifecycleCtx)
	if teammate != nil {
		// External stop is signal-only. Strict resource cleanup must run after
		// sub.Run has stopped producing work, otherwise a final tool call can
		// spawn into an already-reset private job registry. SetCancel also
		// observes a stop request that raced the post-Register setup window.
		teammate.SetCancel(signalCancel)
	}

	parentOut := agent.EventOutFromContext(ctx)
	parentToolUseID := agent.ParentToolUseIDFromContext(ctx)

	// The runner finalizer is an idempotent join boundary. First stop the
	// producer, then (after executeForeground/Background has joined sub.Run)
	// reap every private Bash job, and only then remove its worktree. sync.Once
	// also makes the Execute setup guard and runner defers safe to overlap.
	var finalizeOnce sync.Once
	finalizeDone := make(chan struct{})
	finalize := func() {
		finalizeOnce.Do(func() {
			defer close(finalizeDone)
			signalCancel()
			if childJobs != nil {
				childJobs.ResetAndWait(100 * time.Millisecond)
			}
			if worktreeInfo != nil {
				_ = worktreepkg.Cleanup(worktreeInfo)
			}
		})
		<-finalizeDone
	}
	worktreeCleanupOwnedByExecute = false
	runnerCleanupOwnedByExecute := true
	defer func() {
		if runnerCleanupOwnedByExecute {
			finalize()
		}
	}()
	// A strict session/permission boundary can see the roster entry while the
	// profile and child loop are still being built. SetCancel above converts
	// that previously-lost request into childCtx cancellation. Do not publish a
	// trace start or launch the runner after the boundary has already revoked
	// this child; Execute's guards close the transcript, unregister the roster
	// entry, and join private resources synchronously.
	if err := childCtx.Err(); err != nil {
		return wrapTimeoutErr(err, timeout), nil
	}

	// Signal only after every validation/setup step succeeded and immediately
	// before handing control to the child runner. A denied or short-circuited
	// Agent therefore has no start signal and its trace owner can be discarded
	// as soon as the parent tool_result arrives.
	agent.TraceInvocationStarted(ctx)
	if runInBackground {
		return a.executeBackground(
			sub,
			childCtx,
			finalize,
			parentOut,
			parentToolUseID,
			teammate,
			timeout,
			transcript,
			persistedOnDisk,
			parentNotify,
			func() {
				rosterCleanupOwnedByExecute = false
				runnerCleanupOwnedByExecute = false
				transcriptCleanupOwnedByExecute = false
			},
		)
	}
	runnerCleanupOwnedByExecute = false
	transcriptCleanupOwnedByExecute = false
	return a.executeForeground(sub, childCtx, finalize, parentOut, parentToolUseID, teammate, timeout, transcript, persistedOnDisk)
}

// resolveIsolation handles the `isolation` + `cwd` schema fields
// (G.2, 2026-05-12). Returns the effective sub-agent cwd, an optional
// worktree info struct to clean up on exit, or a user-actionable
// error.
//
// Validation rules (mutually-exclusive + nesting-safe + absolute-path):
//
//  1. `isolation` and `cwd` cannot both be set — the model has to
//     pick one mode, otherwise we'd silently prefer one over the
//     other and surprise the caller.
//  2. `isolation` only accepts "worktree" today; any other value is
//     rejected with a clear hint (claude-code's schema lists
//     "remote" too but that's CCR-only / ant-only, intentionally
//     dropped from Phase G).
//  3. Worktrees refuse to nest — if the parent process is itself
//     inside a worktree, we error out instead of cascading
//     branches that no one can clean up.
//  4. `cwd` MUST be absolute. Relative paths would resolve against
//     whatever cwd happens to be live at sub-loop start, which is
//     racy when N teammates spawn in parallel.
func (a Agent) resolveIsolation(isolation, cwdArg string) (string, *worktreepkg.Info, error) {
	if isolation != "" && cwdArg != "" {
		return "", nil, errors.New("`isolation` and `cwd` are mutually exclusive — pick one")
	}
	if isolation != "" && isolation != "worktree" {
		return "", nil, fmt.Errorf("isolation=%q not supported; only \"worktree\" is recognized", isolation)
	}
	if cwdArg != "" {
		if !strings.HasPrefix(cwdArg, "/") {
			return "", nil, fmt.Errorf("cwd=%q must be an absolute path", cwdArg)
		}
		if fi, err := os.Stat(cwdArg); err != nil {
			return "", nil, fmt.Errorf("cwd=%q: %w", cwdArg, err)
		} else if !fi.IsDir() {
			return "", nil, fmt.Errorf("cwd=%q is not a directory", cwdArg)
		}
		return cwdArg, nil, nil
	}
	if isolation == "worktree" {
		// Refuse nesting — `metis -W feat1` already put us inside a
		// worktree; an Agent({isolation:"worktree"}) here would
		// create a worktree-of-a-worktree which neither git nor our
		// cleanup story handles cleanly.
		cwd, err := os.Getwd()
		if err == nil && worktreepkg.InsideWorktree(cwd) {
			return "", nil, fmt.Errorf("refusing to nest worktree: parent process is already inside a worktree at %s", cwd)
		}
		info, err := worktreepkg.Spawn("") // auto-slug
		if err != nil {
			return "", nil, fmt.Errorf("worktree spawn: %w", err)
		}
		return info.Path, info, nil
	}
	// No isolation requested → inherit parent cwd (return "" so the
	// context key is left unset; tools fall back to os.Getwd()).
	return "", nil, nil
}

// executeForeground is the pre-G.1 behavior: spawn sub-loop in a
// goroutine, drain events synchronously, return the assistant's final
// text as the tool result. The caller blocks until the sub-agent
// finishes.
func (a Agent) executeForeground(
	sub *agent.Loop,
	childCtx context.Context,
	finalize func(),
	parentOut chan<- agent.Event,
	parentToolUseID string,
	teammate *agent.Teammate,
	timeout time.Duration,
	transcript *agent.SubAgentTranscript,
	persistedOnDisk int,
) (resultRet *tools.Result, errRet error) {
	defer transcript.Close()
	defer finalize()
	// G.15 (2026-05-12) — panic recovery for the foreground path.
	// The background path already handles this; the foreground was
	// silently bubbling a panic up to the dispatcher (which would
	// abort the parent turn). Convert to IsError so the parent can
	// see the failure and decide whether to retry.
	defer func() {
		if r := recover(); r != nil {
			resultRet = &tools.Result{
				Output:  fmt.Sprintf("sub-agent panic (recovered): %v", r),
				IsError: true,
			}
			errRet = nil
			if teammate != nil {
				teammate.Finish(agent.StatusFailed, "", fmt.Errorf("panic: %v", r), "panic")
			}
		}
	}()

	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() {
		defer agent.TraceInvocationEnded(childCtx)
		defer close(events)
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("sub-agent run panic: %v", r)
			}
		}()
		done <- sub.Run(childCtx, events)
	}()

	var output strings.Builder
	stopReason := ""
	// G.4 — persistedMsgCount tracks how many of sub.Messages we've
	// already written to disk. Caller seeds the initial value (0 for
	// fresh sub-agents, len(snapshot.Messages) for resumed ones).
	// Bumped on each EventTurnEnd so each turn's new messages get
	// appended exactly once.
	persistedMsgCount := persistedOnDisk
	// Cancellation is recorded immediately, but we keep draining until sub.Run
	// closes events and then join done. Returning at childCtx.Done used to close
	// the roster edge while an InterruptBlock Bash process was still alive.
	var lifecycleErr error
	var eventErr error
	ctxDone := childCtx.Done()
drainLoop:
	for {
		select {
		case <-ctxDone:
			lifecycleErr = childCtx.Err()
			ctxDone = nil
		case ev, ok := <-events:
			if !ok {
				break drainLoop
			}
			forwardSubAgentEvent(parentOut, parentToolUseID, ev)
			switch ev.Kind {
			case agent.EventTextDelta:
				output.WriteString(ev.TextDelta)
				if teammate != nil {
					teammate.AppendText(ev.TextDelta)
				}
			case agent.EventTurnEnd:
				persistedMsgCount = persistNewMessages(transcript, sub, persistedMsgCount)
			case agent.EventPermissionRequest:
				ev.PermissionReply <- agent.PermissionDecisionDeny
			case agent.EventAskUser:
				dismissSubAgentAskUser(ev)
			case agent.EventLoopDone:
				stopReason = ev.StopReason
				persistedMsgCount = persistNewMessages(transcript, sub, persistedMsgCount)
			case agent.EventError:
				if ev.Err != nil && eventErr == nil {
					eventErr = ev.Err
				}
			}
		}
	}
	runErr := <-done
	persistedMsgCount = persistNewMessages(transcript, sub, persistedMsgCount)
	_ = persistedMsgCount
	finalErr := lifecycleErr
	if finalErr == nil {
		finalErr = eventErr
	}
	if finalErr == nil {
		finalErr = runErr
	}
	// Completion is not externally visible until private jobs and worktree
	// resources are gone. The deferred call remains as the panic safety net.
	finalize()

	out := strings.TrimSpace(output.String())
	if finalErr != nil {
		if teammate != nil {
			status := agent.StatusFailed
			hint := stopReason
			if errors.Is(finalErr, context.DeadlineExceeded) && timeout > 0 {
				hint = fmt.Sprintf("timeout %s", timeout)
			} else if errors.Is(finalErr, context.Canceled) {
				status = agent.StatusKilled
				hint = "cancelled"
			}
			teammate.Finish(status, out, finalErr, hint)
		}
		return wrapTimeoutErr(finalErr, timeout), nil
	}
	if agent.IsIncompleteStopReason(stopReason) {
		if out == "" || isEmptyFinalStopReason(stopReason) {
			out = fmt.Sprintf("(sub-agent finished without text output; stop_reason=%s)", stopReason)
		}
		incompleteErr := fmt.Errorf("sub-agent incomplete: %s", stopReason)
		if teammate != nil {
			teammate.Finish(agent.StatusFailed, out, incompleteErr, stopReason)
		}
		return &tools.Result{Output: out, IsError: true}, nil
	}
	if out == "" {
		out = fmt.Sprintf("(sub-agent finished without text output; stop_reason=%s)", stopReason)
	}
	if teammate != nil {
		teammate.Finish(agent.StatusCompleted, out, nil, stopReason)
	}
	return &tools.Result{Output: out}, nil
}

// persistNewMessages appends any sub.History() entries past idx to
// the transcript and returns the new count. nil transcript → no-op.
// On write failure we don't fail the run — persistence is advisory.
func persistNewMessages(t *agent.SubAgentTranscript, sub *agent.Loop, idx int) int {
	if t == nil {
		return idx
	}
	hist := sub.History()
	for i := idx; i < len(hist); i++ {
		_ = t.AppendMessage(hist[i])
	}
	return len(hist)
}

// executeBackground (G.1, 2026-05-12) returns immediately with a
// handshake `{agent_id, name}` tool_result while the actual sub-loop
// runs in a detached goroutine. The model polls SubAgentOutput /
// SubAgentList to read progress and SubAgentStop to terminate.
//
// The goroutine owns the Teammate lifecycle: it appends text deltas
// to teammate.Output as they stream, then calls teammate.Finish on
// exit (Completed / Failed / Killed). The Roster.Unregister is
// likewise deferred inside the goroutine so background sub-agents
// stay visible in /agents list until they actually finish.
//
// parentNotify, when non-nil, receives a SubAgentNotification after
// teammate.Finish — the parent Loop.Run drains this channel at its next
// iter boundary and injects a <sub_agent_idle> reminder (mirrors
// claude-code's idle_notification). nil is fine (tests, loops without
// a subAgentNotify channel).
func (a Agent) executeBackground(
	sub *agent.Loop,
	childCtx context.Context,
	finalize func(),
	parentOut chan<- agent.Event,
	parentToolUseID string,
	teammate *agent.Teammate,
	timeout time.Duration,
	transcript *agent.SubAgentTranscript,
	persistedOnDisk int,
	parentNotify chan<- agent.SubAgentNotification,
	onRunnerStarted func(),
) (*tools.Result, error) {
	if teammate == nil {
		// No Roster wired — graceful fallback to foreground so callers
		// that opt into run_in_background on a Roster-less embedding
		// still get a useful result (just synchronously).
		return a.executeForeground(sub, childCtx, finalize, parentOut, parentToolUseID, nil, timeout, transcript, persistedOnDisk)
	}

	go func() {
		startedAt := time.Now()
		// Unregister closes teammate.done. Declare it first so it executes last,
		// after sub.Run, private jobs, transcript, and worktree have all joined.
		defer func() {
			if a.roster != nil {
				a.roster.UnregisterTeammate(teammate)
			}
		}()
		defer transcript.Close()
		defer finalize()
		// panic recovery — a background sub-agent that panics should
		// land in StatusFailed with a captured error string, not crash
		// the parent process.
		defer func() {
			if r := recover(); r != nil {
				finalize()
				teammate.Finish(agent.StatusFailed, "", fmt.Errorf("panic: %v", r), "panic")
				notifyParent(parentNotify, teammate, time.Since(startedAt))
			}
		}()

		events := make(chan agent.Event, 64)
		done := make(chan error, 1)
		go func() {
			defer agent.TraceInvocationEnded(childCtx)
			defer close(events)
			defer func() {
				if r := recover(); r != nil {
					done <- fmt.Errorf("sub-agent run panic: %v", r)
				}
			}()
			done <- sub.Run(childCtx, events)
		}()

		stopReason := ""
		persistedMsgCount := persistedOnDisk
		var eventErr error
		for ev := range events {
			forwardSubAgentEvent(parentOut, parentToolUseID, ev)
			switch ev.Kind {
			case agent.EventTextDelta:
				teammate.AppendText(ev.TextDelta)
			case agent.EventTurnEnd:
				persistedMsgCount = persistNewMessages(transcript, sub, persistedMsgCount)
			case agent.EventPermissionRequest:
				ev.PermissionReply <- agent.PermissionDecisionDeny
			case agent.EventAskUser:
				dismissSubAgentAskUser(ev)
			case agent.EventLoopDone:
				stopReason = ev.StopReason
				persistedMsgCount = persistNewMessages(transcript, sub, persistedMsgCount)
			case agent.EventError:
				if ev.Err != nil && eventErr == nil {
					eventErr = ev.Err
				}
			}
		}
		runErr := <-done
		persistedMsgCount = persistNewMessages(transcript, sub, persistedMsgCount)
		_ = persistedMsgCount
		finalErr := childCtx.Err()
		if finalErr == nil {
			finalErr = eventErr
		}
		if finalErr == nil {
			finalErr = runErr
		}
		// Resource cleanup is part of completion, not an asynchronous tail.
		// Parent notifications and teammate.done must never precede it.
		finalize()
		final := teammateSnapshotOutput(teammate)
		if finalErr == nil && agent.IsIncompleteStopReason(stopReason) {
			if final == "" || isEmptyFinalStopReason(stopReason) {
				final = fmt.Sprintf("(sub-agent finished without text output; stop_reason=%s)", stopReason)
			}
			finalErr = fmt.Errorf("sub-agent incomplete: %s", stopReason)
		}
		switch {
		case finalErr == nil:
			teammate.Finish(agent.StatusCompleted, final, nil, stopReason)
		case errors.Is(finalErr, context.DeadlineExceeded) && timeout > 0:
			teammate.Finish(agent.StatusFailed, final, finalErr, fmt.Sprintf("timeout %s", timeout))
		case errors.Is(finalErr, context.Canceled):
			teammate.Finish(agent.StatusKilled, final, finalErr, "cancelled")
		default:
			teammate.Finish(agent.StatusFailed, final, finalErr, stopReason)
		}
		notifyParent(parentNotify, teammate, time.Since(startedAt))
	}()
	// The detached runner now owns teammate cleanup. Handoff only after the
	// goroutine exists, so any panic or return before this point leaves cleanup
	// with Execute's guard instead of leaking (or double-unregistering) the slot.
	if onRunnerStarted != nil {
		onRunnerStarted()
	}

	return &tools.Result{
		Output: fmt.Sprintf(
			"sub-agent spawned in background (agent_id=%s, name=%s). Poll progress with SubAgentOutput, list active sub-agents with SubAgentList, terminate with SubAgentStop.",
			teammate.AgentID, teammate.Name,
		),
		Meta: map[string]any{
			"agent_id":   teammate.AgentID,
			"name":       teammate.Name,
			"background": true,
		},
	}, nil
}

// dismissSubAgentAskUser is a defensive fallback for an interaction event
// emitted by a legacy/plugin tool that does not declare
// RequiresUserInteraction. Native interactive tools are rejected by dispatch
// before Execute. Never forward this event to the parent UI: the parent did not
// invoke AskUser and cannot safely attribute the reply to a detached child.
func dismissSubAgentAskUser(ev agent.Event) {
	if ev.AskUserReply == nil {
		return
	}
	select {
	case ev.AskUserReply <- "":
	default:
		// A full or already-resolved channel means the producer no longer needs
		// our fallback. Do not let a malformed plugin event block the consumer.
	}
}

// forwardSubAgentEvent forwards selected sub-agent events to the
// parent's UI channel for live progress rendering. Shared by both
// foreground and background paths so the UX is identical regardless
// of how the sub-agent was spawned.
//
// Fix 1 (2026-05-15) — TextDelta NOT forwarded. The earlier code
// forwarded sub-agent text deltas to the parent's main UI lane
// unprefixed, which interleaved them with the parent's own streaming
// text and corrupted the display (the tmux long-task test showed
// "**统计结果** 该目录下共有 **126** 个" sliced into the middle of
// the parent's reply). The sub-agent's final text shows up
// downstream anyway: it becomes the Agent tool's tool_result body,
// which the parent UI renders as a normal result block. Live deltas
// would need a separate UI lane (claude-code's "Backgrounded agent"
// chip) to be useful; until that lane exists, dropping the delta is
// the right call. Tool starts/results still forward so the parent
// can see "sub: Read", "sub: Grep" in flight.
func forwardSubAgentEvent(parentOut chan<- agent.Event, parentToolUseID string, ev agent.Event) {
	if parentOut == nil {
		return
	}
	switch ev.Kind {
	case agent.EventToolStart, agent.EventToolResult:
		forwarded := ev
		forwarded.ToolName = "sub: " + ev.ToolName
		// Stamp the parent's Agent tool_use_id so the TUI can
		// attribute child progress to the right SubAgentInfo pill —
		// crucial when N sub-agents run in parallel and each
		// generates its own "sub: Read" / "sub: Bash" stream.
		forwarded.SubAgentParentID = parentToolUseID
		// Guard against "send on closed channel". The parent's event
		// channel is closed by the TUI the instant the parent turn's
		// loop.Run returns; a background sub-agent can outlive that
		// turn and still be forwarding tool events. select+default does
		// NOT protect against a send on a closed channel (it only
		// protects against a full buffer), so we recover here. A closed
		// parent channel means the parent TUI is gone anyway, so
		// dropping the event is correct.
		func() {
			defer func() { _ = recover() }()
			select {
			case parentOut <- forwarded:
			default:
				// parent buffer full - drop silently rather than block
			}
		}()
	}
}

// teammateSnapshotOutput is a tiny convenience for the background
// runner: capture whatever's accumulated in the teammate so far when
// we hit an error / cancellation, so callers reading
// SubAgentOutput after a failure still see partial progress.
func teammateSnapshotOutput(t *agent.Teammate) string {
	return strings.TrimSpace(t.Snapshot().Output)
}

// notifyParent sends a SubAgentNotification to the parent loop's notify
// channel after a background sub-agent finishes. Non-blocking (select
// default) — if the channel is full the parent can still discover the
// completion via SubAgentList polling. nil parentNotify is a no-op so
// callers in tests or loops without a subAgentNotify channel don't need
// to guard.
func notifyParent(ch chan<- agent.SubAgentNotification, t *agent.Teammate, dur time.Duration) {
	if ch == nil || t == nil {
		return
	}
	snap := t.Snapshot()
	select {
	case ch <- agent.SubAgentNotification{
		Name:     snap.Name,
		AgentID:  snap.AgentID,
		Status:   snap.Status,
		Summary:  snap.Result,
		Duration: dur,
		Err:      snap.ExitErr,
	}:
	default:
	}
}

func isEmptyFinalStopReason(stopReason string) bool {
	return stopReason == "empty_final_answer" || stopReason == "provider_protocol_error"
}

// wrapTimeoutErr converts a sub-loop error into a user-readable
// tool_result. Pulled out of executeForeground so both
// EventError + `<-done` arms can share the message — without this,
// "timed out after 30s" only appeared on one path.
func wrapTimeoutErr(err error, timeout time.Duration) *tools.Result {
	if errors.Is(err, context.DeadlineExceeded) && timeout > 0 {
		return &tools.Result{
			Output: fmt.Sprintf(
				"sub-agent timed out after %s. Re-spawn with `timeout_seconds: <larger>` if the task legitimately needs more wall-clock budget; otherwise scope down the prompt.",
				timeout,
			),
			IsError: true,
		}
	}
	return &tools.Result{Output: err.Error(), IsError: true}
}
