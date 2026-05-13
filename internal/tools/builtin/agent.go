package builtin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
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

const maxAgentDepth = 3

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
	gate           *permission.Gate
	provider       llm.Provider
	registry       *tools.Registry
	roster         *agent.Roster
	jobsPool       *jobs.Registry // optional; when set, run_in_background works
	model          string
	system         string
	minimalSystem  string // optional; preferred for sub-agent loops to save tokens
	defaultTimeout time.Duration
	// sessionDir is the on-disk directory where sub-agent transcripts
	// are persisted for `/agents resume` + the `resume_from` schema
	// field (G.4, 2026-05-12). Empty = persistence disabled — used by
	// tests and the `metis tools` informational listing path.
	sessionDir string
	// parentSessionID stamps the SubAgentOf field on each sub-agent
	// transcript header so `metis sessions list` can group sub-agents
	// under their spawner. Empty = stand-alone.
	parentSessionID string
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

func (Agent) Name() string { return "Agent" }

// ShortDescription — see Bash.ShortDescription for the rationale.
// Agents calling Agents (recursive fork) is rare and discouraged, so
// the short form omits naming/isolation detail in favor of the
// when-vs-when-NOT heuristic.
func (Agent) ShortDescription() string {
	return "Spawn a sub-agent on a focused task — its own context window, same tools, isolated history. Use for deep code surveys, multi-file scouting, comparative analysis. Don't use for single-Grep lookups or conversation; fork has setup overhead. Brief with goal + context + ruled-out approaches + length cap."
}

func (Agent) Description() string {
	return `Spawn a sub-agent to handle a self-contained, focused task. The sub-agent gets its own message history (no shared context with the parent), the same tool set, and a fresh permission gate cloned from yours. It returns a single final text answer.

Use Agent for:
  - Deep codebase surveys: "find every place we instantiate a Logger and explain the constructor patterns" — the sub-agent can fan out 10+ Grep/Read calls without bloating your context.
  - Multi-file refactor scouting: "list every Go file that imports the old package path" before you start editing.
  - Comparative analysis: "diff how the auth flow works in package A vs package B."
  - Independent verification: "run the test suite and report which tests failed and the smallest reproducing change."

Do NOT use Agent for:
  - Lookups you can do in one or two tool calls: a single Grep, a single Read — just do it inline. Forking has overhead (new context window, new system prompt) that costs more than the search.
  - Conversational tasks: explaining something to the user, formatting an answer, deciding what to do next. The model is you; don't fork to think.
  - Tightly-coupled multi-step work where each step depends on the previous one. Sub-agents can't ask the parent questions; if the work needs back-and-forth, do it inline.

Briefing the sub-agent matters more than briefing yourself:
  - State the goal in one sentence, then provide what you already know and what you've ruled out. The sub-agent starts cold — no memory of this conversation.
  - For lookups, hand over the exact pattern or path. For investigations, hand over the question (prescribed steps become dead weight if the premise is wrong).
  - If you need a short response, say so: "report in under 200 words." Without a cap, sub-agents tend to over-explain.
  - Don't ask the sub-agent to make load-bearing decisions for you. Have it gather evidence; you synthesize.

Naming and isolation:
  - Pass ` + "`name`" + ` to register a named teammate that shows up in /agents and can be addressed via MessageTeammate. Use for long-running parallel workers ("explorer", "verifier"). Same-name collisions are rejected.
  - Pass ` + "`isolation: \"worktree\"`" + ` to give the sub-agent its own git worktree under ~/.metis/worktrees/ — useful for risky experiments that shouldn't touch the parent's checkout. Auto-cleaned on exit. Refused if you're already inside a worktree (no nesting).
  - Pass ` + "`cwd`" + ` to run the sub-agent in a specific directory (mutually exclusive with ` + "`isolation`" + `).
  - Pass ` + "`run_in_background: true`" + ` to fire-and-forget — returns job_id immediately, poll via SubAgentOutput, terminate via SubAgentStop.
  - Pass ` + "`permission_mode`" + ` to override the gate just for this sub-agent (e.g. let an explorer run with "auto" while the parent stays "ask").

Sub-agents inherit ~/.metis/agents/<name>.md or the bundled profile (explore, plan, verify, general, go-reviewer, mcp-debugger, coordinator) when ` + "`name`" + ` matches. Otherwise they use a minimal default system prompt.`
}
func (Agent) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"prompt"},
		"properties": map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "the focused task for the sub-agent",
			},
			"max_iter": map[string]any{
				"type":        "integer",
				"description": "tool-call budget for the sub-agent (default 10)",
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
				"description": "Spawn this sub-agent in an isolated git worktree under ~/.metis/worktrees/. The sub-agent's tools see the worktree as cwd, so file writes don't touch the parent's checkout. Worktree is auto-cleaned when the sub-agent exits (or when parent ctx cancels). Mutually exclusive with `cwd`. Refuses when parent is already inside a worktree (no nesting).",
			},
			"cwd": map[string]any{
				"type":        "string",
				"description": "Absolute path to run the sub-agent in. Overrides the parent's working directory for all filesystem and shell operations within this sub-agent. Mutually exclusive with `isolation: \"worktree\"`.",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Optional name to register this sub-agent in the team roster as. Named teammates show up in /agents list and can be addressed by MessageTeammate({to: name, body: ...}) from other sub-agents. Must start with a letter; only [a-zA-Z0-9._-] allowed; max 32 chars. Two teammates with the same name cannot co-exist.",
			},
			"resume_from": map[string]any{
				"type":        "string",
				"description": "Resume a previously-paused sub-agent by its agent_id (e.g. \"agt-d3a91b07\"). The on-disk transcript is replayed and a fresh sub-loop continues from the last turn. The `prompt` field is used as a follow-up turn appended to the recovered history. Use with care: a sub-agent that's still alive somewhere else WILL cause undefined behavior — resume only after the original sub-agent's run has fully ended (SubAgentList shows it gone, or you killed it via SubAgentStop).",
			},
			"permission_mode": map[string]any{
				"type":        "string",
				"enum":        []string{"ask", "auto", "bypass", "plan", "deny"},
				"description": "Override the permission mode for this sub-agent's gate. The parent's gate is unchanged; the sub-agent gets a clone with its own mode + a fresh denial-streak counter. Useful for letting an explore-agent run with `auto` while the parent stays in `ask`. Omit to inherit the parent's current mode.",
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
	return tools.ConcurrencyExclusive
}

func (a Agent) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := a.gate.Check(context.Background(), "Agent", strFromAny(in["prompt"]))
	return mapDecision(d), src
}

func (a Agent) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	prompt, _ := in["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	if a.provider == nil || a.registry == nil {
		return nil, errors.New("Agent tool not fully wired (missing provider/registry)")
	}
	depth, _ := ctx.Value(agentDepthKey{}).(int)
	if depth >= maxAgentDepth {
		return &tools.Result{
			Output:  fmt.Sprintf("agent nesting limit (%d) exceeded", maxAgentDepth),
			IsError: true,
		}, nil
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
				cap := a.roster.Capacity()
				return &tools.Result{
					Output: fmt.Sprintf(
						"sub-agent capacity exceeded (%d/%d in flight). Wait for a teammate to finish or raise config.Agents.MaxConcurrentSubAgents.",
						a.roster.Count(), cap,
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
		// Background sub-agents detach from Execute's lifetime; the
		// spawned goroutine takes ownership of Unregister. Foreground
		// callers unregister here when Execute returns.
		if !runInBackground {
			defer a.roster.Unregister(teammate.Name)
		}
	}

	// Sub-loop construction is identical in both paths.
	maxIter := intArg(in, "max_iter", 10)
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
	base := a.system
	if a.minimalSystem != "" {
		base = a.minimalSystem
	}
	subSystem := agent.BuildSubPrompt(agent.SubPromptInputs{
		Mode:         subPromptMode,
		Base:         base,
		TeammateName: teammateName,
	})

	// G.9 (2026-05-12) — give the sub-agent its OWN gate so a
	// child's permission-mode flip doesn't leak back into the parent
	// (and so the sub-agent's denial-streak counter doesn't bleed
	// into the parent's circuit breaker). The clone shares rules +
	// classifier — the snapshot is mode + rules at spawn time.
	// Profile-driven mode comes via the schema field `permission_mode`.
	subGate := a.gate.Clone()
	if pm, _ := in["permission_mode"].(string); pm != "" {
		subGate.SetMode(permission.Mode(pm))
	}

	// G.14 (2026-05-12) — per-invocation tool filter. The schema
	// fields `allowed_tools` (allowlist) and `disallowed_tools`
	// (blocklist) narrow the sub-agent's view of the registry
	// without affecting the parent's view. Empty filters = inherit
	// the full parent registry. Filter applies BEFORE the sub-loop
	// is constructed so the sub-agent never sees a tool it
	// shouldn't even try to call.
	allowedTools := stringSliceArg(in, "allowed_tools")
	disallowedTools := stringSliceArg(in, "disallowed_tools")
	subRegistry := a.registry
	if len(allowedTools) > 0 || len(disallowedTools) > 0 {
		subRegistry = filterRegistry(a.registry, allowedTools, disallowedTools)
	}

	sub := agent.NewLoop(a.provider, subRegistry, subGate, agent.NewHookRegistry(), subSystem, maxIter)
	sub.Model = a.model
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
	sub.AppendUser(prompt)
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

	// G.0 timeout — wall-clock cap. Caller-provided `timeout_seconds`
	// wins; else config default; 0 disables.
	timeoutSec := intArg(in, "timeout_seconds", 0)
	timeout := time.Duration(timeoutSec) * time.Second
	if timeoutSec == 0 && a.defaultTimeout > 0 {
		timeout = a.defaultTimeout
	}

	// Build the sub-agent ctx with depth + cwd + (optional) timeout.
	// For the background path the goroutine owns the cancel func and
	// keeps the ctx alive past Execute's return — context.WithCancel
	// doesn't leak when the deferred goroutine runs cancel on its
	// own exit. G.2: subCwd stamps the effective working directory
	// onto the ctx so cwd-aware tools (Bash) inherit it.
	baseCtx := agent.WithCwd(context.WithValue(ctx, agentDepthKey{}, depth+1), subCwd)
	childCtx, cancel := context.WithCancel(baseCtx)
	if timeout > 0 {
		childCtx, cancel = context.WithTimeout(childCtx, timeout)
	}
	if teammate != nil {
		teammate.Cancel = cancel
	}

	parentOut := agent.EventOutFromContext(ctx)

	// G.2 — wrap cancel so the worktree cleanup happens on every
	// exit path (parent ctx cancel, sub-agent natural end, panic in
	// the background goroutine). Foreground path runs cleanup via
	// `defer cancel()` in executeForeground; background path runs it
	// in the goroutine's defer chain.
	if worktreeInfo != nil {
		origCancel := cancel
		cancel = func() {
			origCancel()
			_ = worktreepkg.Cleanup(worktreeInfo)
		}
		if teammate != nil {
			teammate.Cancel = cancel
		}
	}

	if runInBackground {
		return a.executeBackground(sub, childCtx, cancel, parentOut, teammate, timeout, transcript, persistedOnDisk)
	}
	return a.executeForeground(sub, childCtx, cancel, parentOut, teammate, timeout, transcript, persistedOnDisk)
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
	cancel context.CancelFunc,
	parentOut chan<- agent.Event,
	teammate *agent.Teammate,
	timeout time.Duration,
	transcript *agent.SubAgentTranscript,
	persistedOnDisk int,
) (resultRet *tools.Result, errRet error) {
	defer cancel()
	defer transcript.Close()
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
		done <- sub.Run(childCtx, events)
		close(events)
	}()

	var output strings.Builder
	stopReason := ""
	// G.4 — persistedMsgCount tracks how many of sub.Messages we've
	// already written to disk. Caller seeds the initial value (0 for
	// fresh sub-agents, len(snapshot.Messages) for resumed ones).
	// Bumped on each EventTurnEnd so each turn's new messages get
	// appended exactly once.
	persistedMsgCount := persistedOnDisk
	// G.15 (2026-05-12) — select on childCtx.Done() so a parent
	// cancellation tears the drain down even if the sub-loop's
	// event channel deadlocks. Without this guard, a buggy sub-loop
	// could pin the parent turn indefinitely.
drainLoop:
	for {
		select {
		case <-childCtx.Done():
			persistedMsgCount = persistNewMessages(transcript, sub, persistedMsgCount)
			err := childCtx.Err()
			if teammate != nil {
				status := agent.StatusKilled
				hint := "cancelled"
				if errors.Is(err, context.DeadlineExceeded) && timeout > 0 {
					status = agent.StatusFailed
					hint = fmt.Sprintf("timeout %s", timeout)
				}
				teammate.Finish(status, strings.TrimSpace(output.String()), err, hint)
			}
			return wrapTimeoutErr(err, timeout), nil
		case ev, ok := <-events:
			if !ok {
				break drainLoop
			}
			forwardSubAgentEvent(parentOut, ev)
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
			case agent.EventLoopDone:
				stopReason = ev.StopReason
				persistedMsgCount = persistNewMessages(transcript, sub, persistedMsgCount)
			case agent.EventError:
				if ev.Err != nil {
					persistedMsgCount = persistNewMessages(transcript, sub, persistedMsgCount)
					return wrapTimeoutErr(ev.Err, timeout), nil
				}
			}
		}
	}
	if err := <-done; err != nil {
		return wrapTimeoutErr(err, timeout), nil
	}

	out := strings.TrimSpace(output.String())
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
func (a Agent) executeBackground(
	sub *agent.Loop,
	childCtx context.Context,
	cancel context.CancelFunc,
	parentOut chan<- agent.Event,
	teammate *agent.Teammate,
	timeout time.Duration,
	transcript *agent.SubAgentTranscript,
	persistedOnDisk int,
) (*tools.Result, error) {
	if teammate == nil {
		// No Roster wired — graceful fallback to foreground so callers
		// that opt into run_in_background on a Roster-less embedding
		// still get a useful result (just synchronously).
		return a.executeForeground(sub, childCtx, cancel, parentOut, nil, timeout, transcript, persistedOnDisk)
	}

	go func() {
		defer cancel()
		defer transcript.Close()
		defer func() {
			if a.roster != nil {
				a.roster.Unregister(teammate.Name)
			}
		}()
		// panic recovery — a background sub-agent that panics should
		// land in StatusFailed with a captured error string, not crash
		// the parent process.
		defer func() {
			if r := recover(); r != nil {
				teammate.Finish(agent.StatusFailed, "", fmt.Errorf("panic: %v", r), "panic")
			}
		}()

		events := make(chan agent.Event, 64)
		done := make(chan error, 1)
		go func() {
			done <- sub.Run(childCtx, events)
			close(events)
		}()

		stopReason := ""
		persistedMsgCount := persistedOnDisk
		for ev := range events {
			forwardSubAgentEvent(parentOut, ev)
			switch ev.Kind {
			case agent.EventTextDelta:
				teammate.AppendText(ev.TextDelta)
			case agent.EventTurnEnd:
				persistedMsgCount = persistNewMessages(transcript, sub, persistedMsgCount)
			case agent.EventPermissionRequest:
				ev.PermissionReply <- agent.PermissionDecisionDeny
			case agent.EventLoopDone:
				stopReason = ev.StopReason
				persistedMsgCount = persistNewMessages(transcript, sub, persistedMsgCount)
			case agent.EventError:
				if ev.Err != nil {
					persistedMsgCount = persistNewMessages(transcript, sub, persistedMsgCount)
					status := agent.StatusFailed
					hint := stopReason
					if errors.Is(ev.Err, context.DeadlineExceeded) && timeout > 0 {
						hint = fmt.Sprintf("timeout %s", timeout)
					} else if errors.Is(ev.Err, context.Canceled) {
						status = agent.StatusKilled
						hint = "cancelled"
					}
					teammate.Finish(status, teammateSnapshotOutput(teammate), ev.Err, hint)
					return
				}
			}
		}
		err := <-done
		final := teammateSnapshotOutput(teammate)
		switch {
		case err == nil:
			teammate.Finish(agent.StatusCompleted, final, nil, stopReason)
		case errors.Is(err, context.DeadlineExceeded) && timeout > 0:
			teammate.Finish(agent.StatusFailed, final, err, fmt.Sprintf("timeout %s", timeout))
		case errors.Is(err, context.Canceled):
			teammate.Finish(agent.StatusKilled, final, err, "cancelled")
		default:
			teammate.Finish(agent.StatusFailed, final, err, stopReason)
		}
	}()

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

// forwardSubAgentEvent forwards selected sub-agent events to the
// parent's UI channel for live progress rendering. Shared by both
// foreground and background paths so the UX is identical regardless
// of how the sub-agent was spawned.
func forwardSubAgentEvent(parentOut chan<- agent.Event, ev agent.Event) {
	if parentOut == nil {
		return
	}
	switch ev.Kind {
	case agent.EventToolStart, agent.EventToolResult, agent.EventTextDelta:
		forwarded := ev
		if ev.Kind == agent.EventToolStart || ev.Kind == agent.EventToolResult {
			forwarded.ToolName = "sub: " + ev.ToolName
		}
		select {
		case parentOut <- forwarded:
		default:
			// parent buffer full — drop silently rather than block
		}
	}
}

// teammateSnapshotOutput is a tiny convenience for the background
// runner: capture whatever's accumulated in the teammate so far when
// we hit an error / cancellation, so callers reading
// SubAgentOutput after a failure still see partial progress.
func teammateSnapshotOutput(t *agent.Teammate) string {
	return strings.TrimSpace(t.Snapshot().Output)
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
