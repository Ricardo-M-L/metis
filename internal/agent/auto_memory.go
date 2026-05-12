package agent

// auto_memory.go — extractMemories v2 (full Claude Code parity).
//
// What changed from v1 (the 165-line "basic" version this replaces):
//
//   - Triggered by EmitLoopEnd (not turn % 10). The hook fires when the
//     model's most recent reply contains no further tool_use, i.e. the
//     query loop naturally finished — same boundary openclaude uses.
//   - Runs RunForkedAgent (not a one-shot Complete). The fork has
//     Read/Grep/Glob/Bash(read-only)/Edit/Write(memdir) so it can
//     inspect existing memos, dedup, and write per-topic .md files
//     itself.
//   - Cursor + mutual-exclusion: tracks the last message index processed,
//     skips when the main agent has already touched the memdir this turn,
//     stashes pending context for trailing run when an extraction is
//     in-flight.
//   - Storage: per-topic markdown files with frontmatter under
//     ~/.metis/memory/, plus an index in MEMORY.md regenerated after
//     every run. Replaces the single in-process auto_memory block.
//
// What stays:
//
//   - Best-effort: any error is logged once and swallowed; auto-memory
//     never crashes the main turn.
//   - Off by default: only fires when Loop.AutoMemory == true.
//
// What's deliberately not here yet:
//
//   - autoDream cross-session consolidation (Phase F, optional).
//   - Team memdir (TEAMMEM feature flag in openclaude). Out of scope.
//   - GrowthBook-style flag-controlled throttle. We use a simple
//     min-interval; metis has no flag service.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memdir"
)

// AutoMemoryEvery legacy constant — kept so the v1 callers compile,
// but ignored in v2 (the trigger is now LoopEnd-based, not turn-count).
const AutoMemoryEvery = 10

// AutoMemoryMinInterval is the minimum wall-clock between extractions.
// Without it a chatty user can fire extractions every few seconds; the
// bound caps spend at ~one fork-agent burst per minute. Override per
// loop via Loop.AutoMemoryEvery (interpreted as seconds when v2-mode).
const AutoMemoryMinInterval = 60 * time.Second

// MaxExtractorTurns caps API round-trips inside the fork. 5 matches
// openclaude — well-behaved extractions complete in 2-4 (read manifest
// → optionally Read existing files → Edit/Write 1+ files → done).
const MaxExtractorTurns = 5

// DreamPhase mirrors claude-code's DreamTask.ts state model (G.5,
// 2026-05-12). The extractor walks through these phases on every fork
// so /dream status can show "currently scanning conversation" vs
// "writing 3 memory files" without the user having to grep
// debug.log. Values are stable for tooling.
type DreamPhase string

const (
	DreamPhaseIdle       DreamPhase = "idle"
	DreamPhaseStarting   DreamPhase = "starting"
	DreamPhaseExtracting DreamPhase = "extracting"
	DreamPhaseWriting    DreamPhase = "writing"
	DreamPhaseDone       DreamPhase = "done"
)

// DreamNotification is delivered on Loop.DreamNotify when a
// background extraction completes. The Loop drains the channel at
// iteration boundaries (mirrors job_notify.go) and synthesizes a
// <memory_consolidation_done> system-reminder for the model.
type DreamNotification struct {
	Phase        DreamPhase
	FilesTouched []string
	Duration     time.Duration
	SessionCount int
	Err          error
}

// AutoMemoryExtractor is the long-lived helper attached to a Loop. It
// owns the cursor + in-flight + pending state that openclaude calls
// "closure-scoped state" — keeping it on the Loop (not module-global)
// means tests can construct fresh extractors per scenario without
// bleed-through.
type AutoMemoryExtractor struct {
	loop *Loop

	mu sync.Mutex

	// lastProcessedIdx is the index in Loop.Messages up to which we've
	// already considered this session. The next extraction only sees
	// new messages after this point.
	lastProcessedIdx int

	// inProgress is true while a fork is running. A second EmitLoopEnd
	// during a fork stashes its context as `pending` and runs once the
	// current one finishes (trailing run pattern).
	inProgress bool

	// pending is the latest stashed turn awaiting a trailing run.
	// nil when nothing's queued.
	pending bool

	// lastFiredAt rate-limits extractions even when the user fires
	// turns rapidly — without this an "ok / ok / ok" exchange would
	// burn an extraction each.
	lastFiredAt time.Time

	// totalExtractions is a session-lifetime counter for debug/log.
	totalExtractions int

	// memdirRoot caches the resolved memdir path so we don't hit
	// os.UserHomeDir per invocation.
	memdirRoot string

	// Phase is the current DreamTask state (G.5). Tracked alongside
	// the legacy inProgress bool so existing call sites stay
	// unchanged while /dream status can read the finer-grained
	// state. Idle when no fork is active.
	phase DreamPhase

	// lastFilesTouched lists which memory files were modified by the
	// most recent fork (computed by diffing memdir contents before vs
	// after). Snapshotted into ExtractorStats so /dream can show
	// "wrote feedback_chinese_replies.md, project_release_freeze.md".
	lastFilesTouched []string

	// lastDuration is the wall-clock duration of the most recent
	// extraction (start of runOnce → end). Used by /dream status to
	// help the user judge if extractions are blowing past expected
	// budgets.
	lastDuration time.Duration

	// dreamNotify is the optional outbound channel. When non-nil the
	// extractor posts a DreamNotification on every completed fork
	// (success or failure), so the parent Loop can inject a
	// <memory_consolidation_done> system-reminder on its next iter.
	dreamNotify chan<- DreamNotification
}

// NewAutoMemoryExtractor wires an extractor to a loop. memdirRoot
// defaults to memdir.DefaultRoot() if empty (the production path);
// tests pass a t.TempDir() to keep the host's real memdir clean.
func NewAutoMemoryExtractor(loop *Loop, memdirRoot string) (*AutoMemoryExtractor, error) {
	if loop == nil {
		return nil, fmt.Errorf("auto-memory: nil loop")
	}
	if memdirRoot == "" {
		root, err := memdir.DefaultRoot()
		if err != nil {
			return nil, err
		}
		memdirRoot = root
	}
	if err := memdir.EnsureRoot(memdirRoot); err != nil {
		return nil, err
	}
	return &AutoMemoryExtractor{
		loop:       loop,
		memdirRoot: memdirRoot,
	}, nil
}

// MemdirRoot returns the resolved memdir path the extractor writes
// to. Exposed for the /memory slash command + tests.
func (e *AutoMemoryExtractor) MemdirRoot() string { return e.memdirRoot }

// SetDreamNotify wires the outbound notification channel. The Loop
// owns the channel and drains it on iteration boundaries.
// Idempotent — passing nil disables notifications without disturbing
// in-flight work. (G.5, 2026-05-12)
func (e *AutoMemoryExtractor) SetDreamNotify(ch chan<- DreamNotification) {
	e.mu.Lock()
	e.dreamNotify = ch
	e.mu.Unlock()
}

// Stats returns a point-in-time snapshot of extractor state.
type ExtractorStats struct {
	LastProcessedIdx int
	TotalExtractions int
	InProgress       bool
	Pending          bool
	LastFiredAt      time.Time
	// G.5 — DreamTask state model.
	Phase            DreamPhase
	LastFilesTouched []string
	LastDuration     time.Duration
}

func (e *AutoMemoryExtractor) Stats() ExtractorStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	phase := e.phase
	if phase == "" {
		phase = DreamPhaseIdle
	}
	// Copy slice so callers can't race with a future runOnce mutating
	// e.lastFilesTouched while they format their /dream output.
	var ft []string
	if len(e.lastFilesTouched) > 0 {
		ft = append(ft, e.lastFilesTouched...)
	}
	return ExtractorStats{
		LastProcessedIdx: e.lastProcessedIdx,
		TotalExtractions: e.totalExtractions,
		InProgress:       e.inProgress,
		Pending:          e.pending,
		LastFiredAt:      e.lastFiredAt,
		Phase:            phase,
		LastFilesTouched: ft,
		LastDuration:     e.lastDuration,
	}
}

// OnLoopEnd is the hook entry point — wire it onto Loop.Hooks via
// pubhook.LoopEndHandler so it fires at every natural turn boundary.
//
// stop is the loop's stopReason ("end_turn", "no_tool_calls",
// "halted_by_hook", …). We only run for "end_turn" and "no_tool_calls"
// — the other reasons mean the model didn't finish a coherent answer
// and extracting from it would just amplify noise.
//
// Runs synchronously inside the hook callback BUT spawns a goroutine
// for the actual fork — the hook must return promptly because it's
// called on the loop's hot path. A 60s+ extractor fork would block
// the main loop's return otherwise.
func (e *AutoMemoryExtractor) OnLoopEnd(ctx context.Context, stop string) {
	if !e.loop.AutoMemory {
		return
	}
	if stop != "end_turn" && stop != "no_tool_calls" {
		return
	}

	e.mu.Lock()
	if e.inProgress {
		// Stash and bail — the in-flight extractor will pick up our
		// changes via its trailing run.
		e.pending = true
		e.mu.Unlock()
		return
	}
	if !e.lastFiredAt.IsZero() && time.Since(e.lastFiredAt) < AutoMemoryMinInterval {
		// Throttled — but still process eventually. A user with a
		// long-running session would otherwise lose extraction
		// chances forever just because they fired turns fast early on.
		// We mark pending so the next post-throttle turn flushes.
		e.pending = true
		e.mu.Unlock()
		return
	}
	if e.mainAgentTouchedMemdirSinceLocked() {
		// Mutual exclusion: when the main agent (the user driving the
		// session, with a model that may have memdir-aware system
		// prompts) wrote to memdir directly, skip the fork to avoid
		// double-writing the same insight in two flavours. Advance
		// the cursor so future runs only consider messages after this
		// point.
		e.advanceCursorLocked()
		e.mu.Unlock()
		return
	}
	e.inProgress = true
	e.lastFiredAt = time.Now()
	e.mu.Unlock()

	// Detach: the hook return must not be blocked by the fork. The
	// fork uses a fresh background context with timeout — the parent's
	// ctx may be cancelled the moment the user starts typing again.
	//
	// Bump the inflight counter HERE (not inside runOnce/RunForked)
	// because waitForkInflight in cmdRun starts polling the moment
	// EmitLoopEnd returns. If we wait for the goroutine to schedule
	// before incrementing, the process can exit before the fork has
	// a chance to enter Provider.Complete.
	incInflight()
	go func() {
		defer decInflight()
		e.runOnce(ctx)
	}()
}

// runOnce performs one fork-extraction cycle: snapshot parent state,
// build the prompt + manifest, call RunForkedAgentInstrumented,
// regenerate MEMORY.md from the post-fork directory, then service any
// pending trailing run.
//
// G.5 (2026-05-12) — tracks DreamTask phase transitions
// (starting → extracting → writing → done) and posts a
// DreamNotification on the Loop's outbound channel so the model
// gets a <memory_consolidation_done> reminder on the next iter.
func (e *AutoMemoryExtractor) runOnce(parentCtx context.Context) {
	startedAt := time.Now()
	// G.5 phase tracking — set "starting" up-front so /dream status
	// shows activity even before the fork actually launches.
	e.setPhase(DreamPhaseStarting)

	// Snapshot the memdir's pre-state so we can diff it against the
	// post-state and surface which files the fork actually touched.
	// Use Background — the scan is filesystem-only and ms-fast, and
	// we don't want parentCtx cancellation to leave us with a
	// half-computed diff that wrongly attributes files as "touched".
	pre, _ := snapshotMemdirNames(context.Background(), e.memdirRoot)

	var runErr error
	defer func() {
		duration := time.Since(startedAt)
		post, _ := snapshotMemdirNames(context.Background(), e.memdirRoot)
		touched := diffMemdirNames(pre, post)

		e.mu.Lock()
		e.inProgress = false
		e.phase = DreamPhaseDone
		e.lastDuration = duration
		e.lastFilesTouched = touched
		shouldTrail := e.pending
		e.pending = false
		notifyCh := e.dreamNotify
		sessionCount := e.totalExtractions
		e.mu.Unlock()

		// Best-effort notify — drop on full channel rather than
		// block the runOnce defer chain. Mirrors the bash job pool
		// notification pattern (jobs.Registry).
		if notifyCh != nil {
			select {
			case notifyCh <- DreamNotification{
				Phase:        DreamPhaseDone,
				FilesTouched: append([]string(nil), touched...),
				Duration:     duration,
				SessionCount: sessionCount,
				Err:          runErr,
			}:
			default:
			}
		}

		if shouldTrail {
			// Queue a trailing run on a fresh background ctx so a
			// cancelled parent doesn't take the trailing extraction
			// down with it. We re-enter via OnLoopEnd("end_turn") to
			// reuse all the same gates.
			go e.OnLoopEnd(context.Background(), "end_turn")
		}
	}()

	// Build snapshot under a defensive timeout — the snapshot itself
	// is cheap, but Provider.Complete inside the fork can take 10s+.
	ctx, cancel := context.WithTimeout(detach(parentCtx), 90*time.Second)
	defer cancel()

	snap := SnapshotForFork(e.loop)
	if snap == nil {
		runErr = fmt.Errorf("snapshot returned nil")
		return
	}

	files, _ := memdir.ScanMemoryFiles(ctx, e.memdirRoot)
	manifest := memdir.FormatManifest(e.memdirRoot, files)

	prompt := buildExtractorPrompt(e.memdirRoot, manifest)

	gate := CreateAutoMemCanUseTool(e.memdirRoot, e.loop.Registry)

	// G.5 — flip to "extracting" once we actually invoke the fork.
	// The fork itself can take 10-60s on slow providers; /dream
	// status shows this phase for most of that wall-clock budget.
	e.setPhase(DreamPhaseExtracting)
	res, err := RunForkedAgent(ctx, ForkedAgentParams{
		Cache:      *snap,
		Prompt:     prompt,
		CanUseTool: gate,
		Registry:   e.loop.Registry,
		MaxTurns:   MaxExtractorTurns,
		Hooks:      e.loop.Hooks,
		ForkLabel:  "extract_memories",
		Memory:     e.loop.Memory,
	})
	if os.Getenv("METIS_AUTO_MEMORY_DEBUG") == "1" {
		if err != nil {
			fmt.Fprintf(os.Stderr, "[auto-memory] fork error: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr,
				"[auto-memory] fork done: turns=%d stop=%s in_tok=%d out_tok=%d cache_read=%d cache_create=%d duration=%s\n",
				res.Turns, res.StopReason, res.Usage.InputTokens, res.Usage.OutputTokens,
				res.Usage.CacheReadInputTokens, res.Usage.CacheCreationInputTokens, res.Duration)
		}
	}
	if err != nil {
		// Best-effort: we don't surface to the user. Extractor errors
		// (network, provider 4xx, gate-denial-then-stuck) are logged
		// to debug.log via the loop's existing transport logger.
		runErr = err
		return
	}

	// G.5 — "writing" phase covers the cursor bump + index
	// regeneration that finalises the fork's output to disk.
	e.setPhase(DreamPhaseWriting)
	e.mu.Lock()
	e.totalExtractions++
	e.advanceCursorLocked()
	e.mu.Unlock()

	// Refresh the index AFTER the fork wrote any new files. The model
	// may have edited MEMORY.md itself (we allow Edit/Write inside
	// memdir, including the index), but regenerating from disk gives
	// us a canonical, sorted, dedup'd version that's robust to the
	// model writing `* foo` instead of `- [foo](foo.md)`.
	if err := regenerateIndex(ctx, e.memdirRoot); err != nil {
		// Non-fatal: the next extraction will retry.
		_ = err
	}

	// Optional: stamp the result for analytics. logEvent isn't wired
	// here (metis has no central event bus equivalent to openclaude's
	// growthbook stack), so we rely on the SubagentStop hook the fork
	// already emitted to surface usage.
	_ = res
}

// setPhase atomically transitions the DreamTask phase. Used as a
// helper inside runOnce so we can sprinkle phase markers without
// touching e.mu directly at every transition site.
func (e *AutoMemoryExtractor) setPhase(p DreamPhase) {
	e.mu.Lock()
	e.phase = p
	e.mu.Unlock()
}

// snapshotMemdirNames returns the set of `.md` basenames under root
// at the time of the call. Used by runOnce to compute which files
// the fork touched (created / modified). Filenames only — we don't
// keep mtimes because the diff cares about presence, and Edit can
// rewrite a file with identical content.
func snapshotMemdirNames(ctx context.Context, root string) (map[string]struct{}, error) {
	files, err := memdir.ScanMemoryFiles(ctx, root)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(files))
	for _, f := range files {
		out[filepath.Base(f.Path)] = struct{}{}
	}
	return out, nil
}

// diffMemdirNames returns the basenames of every file that's either
// in `post` but not `pre` (newly written) or in `pre` but not `post`
// (deleted). Sorted for stable rendering in /dream status output.
func diffMemdirNames(pre, post map[string]struct{}) []string {
	if pre == nil && post == nil {
		return nil
	}
	seen := make(map[string]struct{})
	for k := range pre {
		if _, ok := post[k]; !ok {
			seen[k] = struct{}{}
		}
	}
	for k := range post {
		if _, ok := pre[k]; !ok {
			seen[k] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// advanceCursorLocked moves lastProcessedIdx to the current
// Loop.Messages length so the next extraction only considers new
// messages.
func (e *AutoMemoryExtractor) advanceCursorLocked() {
	e.loop.mu.RLock()
	e.lastProcessedIdx = len(e.loop.Messages)
	e.loop.mu.RUnlock()
}

// mainAgentTouchedMemdirSinceLocked scans Loop.Messages from
// lastProcessedIdx forward, looking for an Edit/Write tool_use whose
// file_path lies inside memdirRoot. If found, the main agent already
// did the extractor's job — we should defer to it.
func (e *AutoMemoryExtractor) mainAgentTouchedMemdirSinceLocked() bool {
	e.loop.mu.RLock()
	defer e.loop.mu.RUnlock()
	for i := e.lastProcessedIdx; i < len(e.loop.Messages); i++ {
		m := e.loop.Messages[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type != "tool_use" {
				continue
			}
			if b.ToolName != "Edit" && b.ToolName != "Write" && b.ToolName != "MultiEdit" {
				continue
			}
			path := stringField(b.ToolInput, "file_path")
			if path == "" {
				path = stringField(b.ToolInput, "path")
			}
			if memdir.IsAutoMemPath(e.memdirRoot, path) {
				return true
			}
		}
	}
	return false
}

// regenerateIndex rebuilds MEMORY.md from the current set of memdir
// files. Idempotent: deterministic ordering means reading and
// re-writing without changes is a no-op.
func regenerateIndex(ctx context.Context, root string) error {
	files, err := memdir.ScanMemoryFiles(ctx, root)
	if err != nil {
		return err
	}
	return memdir.WriteIndex(root, files)
}

// detach returns a fresh context.Background that's NOT a child of
// parent. We need this because the parent ctx is the request ctx that
// gets cancelled when the user types the next message; the extractor
// fork should keep running.
//
// We DO copy any cancel-on-shutdown signals from parent if the caller
// set them — but that's a future enhancement; today we just ignore
// parent's lifetime entirely.
func detach(parent context.Context) context.Context {
	_ = parent
	return context.Background()
}

// buildExtractorPrompt is the user-side message handed to the forked
// agent. The system prompt is the parent's (snap.System), so the
// extractor inherits all the user's working context — this prompt
// just tells it what we want done.
//
// Designed to be < 1500 tokens so even on cold cache the per-extraction
// cost is bounded.
func buildExtractorPrompt(memdirRoot, manifest string) string {
	abs, _ := filepath.Abs(memdirRoot)
	if abs == "" {
		abs = memdirRoot
	}
	var sb strings.Builder
	sb.WriteString("This is an auto-memory extraction sub-task.\n\n")
	sb.WriteString("**Goal**: scan the conversation above for *user-stable* facts worth keeping across sessions, and persist them to the memory directory.\n\n")
	sb.WriteString("**Memory directory**: `")
	sb.WriteString(abs)
	sb.WriteString("/`\n\n")
	sb.WriteString("**Existing memories**:\n\n")
	sb.WriteString(manifest)
	sb.WriteString("\n\n")
	sb.WriteString(`**What to save**:

- ` + "`user`" + ` — durable facts about who the user is (role, expertise, ongoing responsibilities). Rarely changes.
- ` + "`feedback`" + ` — guidance about how to approach work (do this, don't do that). Always include the *why* the user gave so judgment calls in edge cases stay possible.
- ` + "`project`" + ` — initiatives, decisions, deadlines that aren't derivable from code or git. Convert relative dates ("Thursday") to absolute (YYYY-MM-DD).
- ` + "`reference`" + ` — pointers into external systems (Linear projects, Slack channels, dashboards).

**What NOT to save**:

- Code patterns, architecture, or paths — derivable by reading the project.
- Git history or who-changed-what — ` + "`git log`" + ` is authoritative.
- Debugging fixes — the fix is in the code; the commit message has the why.
- Ephemeral task state, in-progress work, today's bug.
- Anything already in an existing memory above.

**Process**:

1. Skim the conversation above to identify candidate facts.
2. For each, check the manifest above to see if a relevant memory already exists. If yes and the fact materially extends or corrects it, ` + "`Edit`" + ` that file. If yes and the fact is already covered, skip — don't duplicate.
3. For genuinely new facts, ` + "`Write`" + ` a new file in the memory directory:
   - Filename: ` + "`<type>_<topic>.md`" + ` (e.g. ` + "`user_role.md`" + `, ` + "`feedback_chinese_replies.md`" + `, ` + "`project_release_freeze.md`" + `).
   - Frontmatter (YAML):
     ` + "```yaml" + `
     ---
     name: <short title>
     description: <one-line hook used in the index>
     type: user|feedback|project|reference
     ---
     ` + "```" + `
   - Body: 1-3 short paragraphs. For ` + "`feedback`" + ` and ` + "`project`" + ` types, lead with the rule/fact, then **Why:** and **How to apply:** lines.
4. If nothing new is worth saving, do NOT write any file — just reply with a one-line summary.

**Constraints**:

- You can only ` + "`Read`" + `/` + "`Grep`" + `/` + "`Glob`" + ` anywhere, and ` + "`Edit`" + `/` + "`Write`" + ` only inside the memory directory.
- Keep the total output minimal — this is background work, not a user-facing reply.
- Do NOT modify ` + "`MEMORY.md`" + ` directly; it's regenerated automatically after you finish.

Begin.`)
	return sb.String()
}

// parseAutoMemoryLines extracts `key: value` lines from a string blob.
// Retained from v1 because (a) the v1 _test pins it and they're still
// useful coverage of the parser, and (b) future paths that want a
// cheap key-value extraction (e.g. a `metis memory note <text>` CLI
// shortcut that runs the LLM with the v1 prompt) can call this.
//
// NONE → nil. Lines without a colon are dropped (conversational
// filler). Bullets / numbering are stripped from the start.
func parseAutoMemoryLines(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" || strings.EqualFold(text, "NONE") {
		return nil
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimLeft(line, "-*0123456789. ")
		if !strings.Contains(line, ":") {
			continue
		}
		out = append(out, line)
	}
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

// MaybeExtractMemory is the v1 compatibility shim. Old call sites that
// invoked Loop.MaybeExtractMemory(ctx) directly (rare; the trigger
// hook is the proper integration point) still work — we just route to
// the new extractor.
//
// Returns 0 always (the v1 contract was "number of facts extracted",
// but in v2 the fork writes files directly and we don't re-parse the
// new file count synchronously here). Callers needing the count should
// use Stats() / scan the memdir.
func (l *Loop) MaybeExtractMemory(ctx context.Context) int {
	if l == nil || !l.AutoMemory {
		return 0
	}
	if l.autoMemExtractor == nil {
		ext, err := NewAutoMemoryExtractor(l, "")
		if err != nil {
			return 0
		}
		l.autoMemExtractor = ext
	}
	l.autoMemExtractor.OnLoopEnd(ctx, "end_turn")
	return 0
}
