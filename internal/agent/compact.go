// Package compact implements context window compaction.
// Inspired by Claude Code's services/compact/ and Hermes' sliding window
// with protect_first_n/last_n. When context reaches a threshold fraction
// of the model's max tokens, old messages are summarized into boundary
// turns, keeping recent context intact.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// errIsEOF returns true for the io.EOF the streaming providers signal
// when the SSE channel closes cleanly. Kept inline so summarize stays
// readable; bigger compaction package isn't worth a util file.
func errIsEOF(err error) bool { return errors.Is(err, io.EOF) }

// Config holds compaction thresholds.
type Config struct {
	Threshold        float64 // fraction of context window for full Compact (e.g. 0.85)
	ProtectFirst     int     // always keep first N messages
	ProtectLast      int     // always keep last N messages
	MaxSummaryTokens int     // max tokens in a summary turn

	// SnipThreshold is the lighter-weight threshold for tool-result
	// truncation (Snip). Triggered BEFORE Compact so a context that's
	// merely heavy with tool dumps gets cleaned up cheaply (no LLM
	// call) instead of going straight to summarization. Mirrors
	// claude-code's snip → microcompact → autocompact tiering. Set to
	// 0 to disable snip entirely.
	SnipThreshold float64

	// SnipMaxToolResultChars caps each tool_result block's char length
	// during a Snip pass. Anything longer is truncated with a
	// "[snipped: NNN chars]" marker. The model still sees the
	// tool_use_id linkage and the result's first chunk, which is
	// usually enough to remember "I called grep on X and it found
	// stuff". Default 800 chars (~200 tokens) covers the common case
	// where the agent already extracted what it needed.
	SnipMaxToolResultChars int

	// MicrocompactDir is where Microcompact writes off-loaded tool
	// output. Empty disables microcompact entirely. Default
	// "~/.metis/cache/<sessionID>" via runtime config wiring.
	MicrocompactDir string

	// MicrocompactMinChars is the per-block size threshold above
	// which Microcompact offloads to disk (vs Snip's smaller cap).
	// Microcompact preserves recoverability — the model can ask
	// the runtime to read the cached file via Read tool — whereas
	// Snip is permanently lossy. So we set a higher threshold
	// (default 4000) to reserve disk-write overhead for genuinely
	// large outputs that future iterations might need verbatim.
	MicrocompactMinChars int

	// CollapseThreshold is the threshold for context-collapse: a
	// mid-conversation summary that folds messages [ProtectFirst+1 ..
	// ProtectFirst+CollapseFoldWindow] into a single boundary turn.
	// Differs from full Compact in that it preserves the LATEST
	// messages too (Compact keeps both ends; Collapse only folds the
	// "old middle"). Set to 0 to disable collapse.
	CollapseThreshold float64

	// CollapseFoldWindow is the number of messages from the start
	// (after ProtectFirst) that get folded into a summary when
	// Collapse triggers. Default 10 — folds early-conversation
	// context into a single summary, leaving recent + initial
	// messages intact.
	CollapseFoldWindow int

	// ProtectedTools is the set of tool names whose `tool_result`
	// blocks Snip / SnipAll / Microcompact must NOT mutate. Mirrors
	// opencode's PRUNE_PROTECTED_TOOLS and goalfymax's
	// FRAMEWORK_RESERVED_ARGS — for tools whose output is itself a
	// summary or a long-term reference the model later cites
	// verbatim (memory recall, skill help, file reads the model is
	// actively planning around), Snip-style "[snipped: N chars]"
	// markers would silently erase the very evidence the model is
	// supposed to anchor on. Match is case-insensitive on tool name.
	// Empty disables the whitelist (legacy behaviour).
	ProtectedTools []string

	// RedactSecrets, when true, runs each summarized message
	// through a regex pass that replaces likely API keys / tokens /
	// passwords with `[REDACTED]` BEFORE the text is shipped to the
	// summarizer LLM. Two reasons:
	//
	//  1. Summary boundaries get persisted to disk (session JSONL)
	//     and replayed across reloads — leaking a key once would
	//     bake it into every future turn's prompt.
	//  2. Mirrors hermes-agent's `_SECRET_PATTERNS` scrub: the
	//     summarizer otherwise has zero reason to know the
	//     concrete key, it just needs to know "auth was set up".
	//
	// Default true; turn off only in tests or when caller has
	// already redacted upstream.
	RedactSecrets bool

	// IterativeSummary, when true, makes Compact reuse the previous
	// summary string (stored on Compactor.LastSummary) as the seed
	// for the next summarize() call: the LLM is asked to UPDATE the
	// prior summary with the new middle messages rather than
	// re-summarize the whole history. Mirrors hermes-agent's
	// context_compressor.py iterative mode (Active Task >
	// Completed Actions > Resolved Questions priority).
	//
	// Default true. Turn off only when you specifically want a
	// fresh take each Compact (debugging summarizer drift).
	IterativeSummary bool

	// MaxSummaryRetries is the number of additional streaming
	// summarize() attempts after the first one fails. Each retry
	// uses jittered backoff (50..200ms × attempt). After all
	// streaming retries are exhausted, one last attempt is made
	// via Provider.Complete() (non-streaming) before bubbling the
	// error up to recordCompactResult.
	//
	// Default 2 → up to 3 streaming attempts + 1 non-streaming.
	// Set to 0 to keep legacy "fail on first error" behaviour.
	MaxSummaryRetries int

	// IdleMaxSeconds — wall-clock seconds since the last microcompact
	// (or Loop start) after which maybeCompact force-runs a Microcompact
	// pass regardless of whether SnipThreshold is crossed. Mirrors
	// claude-code's services/compact/timeBasedMCConfig.ts (which kicks
	// in roughly at the 60-min provider-cache TTL boundary, on the
	// theory that tool_results older than the cache TTL are no longer
	// helping the cache and shouldn't keep wasting wire bytes). Off
	// when 0 — token-only triggering. Default 3600 (1h).
	IdleMaxSeconds int

	// KeepRecentToolResults — number of most-recent tool_result blocks
	// (anywhere in the eligible Microcompact range, not just the trailing
	// ProtectLast messages) that must NOT be offloaded. Complements
	// ProtectLast: if the trailing 5 messages happen to be short
	// user/assistant chatter ("继续", "ok"), the previous batch of
	// tool_results would otherwise get evicted to disk even though
	// they're still the most recent evidence the model is working
	// against. Mirrors CC's services/compact/microCompact.ts keepRecent
	// (default 5). Set 0 to disable — only ProtectLast applies then.
	KeepRecentToolResults int

	// MinimumTokens — absolute lower bound (in estimated tokens). Full
	// Compact will NOT fire below this even if the percentage threshold
	// is crossed. Mirrors DeepSeek-TUI's MINIMUM_AUTO_COMPACTION_TOKENS
	// floor (compaction.rs:29): on a 1M-window provider the 0.95 trigger
	// is 950K, which is great, but on a fresh session you don't want
	// metis rewriting prefix-cache anchors over a 60K-token convo just
	// because the user's max_tokens config is low. Set 0 to disable —
	// pre-2026-05-16 behaviour (percent-only). The DefaultCompactionConfig
	// keeps 0 for backwards compatibility with unit tests; production
	// wiring (runtime/agent_loop.go) injects the value from
	// Session.AutoCompactMinimumTokens (toml `auto_compact_minimum_tokens`,
	// default 50_000).
	MinimumTokens int
}

// DefaultConfig returns sensible defaults.
func DefaultCompactionConfig() Config {
	return Config{
		// 0.95 (up from 0.85 on 2026-05-16): the 2026-05-16 longrun
		// against DeepSeek-V4-Pro (1M context) hit 1.3M tokens per
		// turn before the 0.85 trigger would have fired, because the
		// per-turn peak still sat under cap*0.85 = 850K. Raising the
		// gate to 0.95 means compaction now fires at ~950K — close
		// enough to the cap that the LLM-side recovery (transport
		// overflow auto-retry) catches the rare overshoot, and the
		// prompt cache survives much longer between rewrites. On
		// smaller windows (128K), 0.95 = 121.6K, which is roughly
		// what the old 0.85 default (108K) gave anyway minus the
		// per-request output reservation.
		Threshold:              0.95,
		ProtectFirst:           1, // system message
		ProtectLast:            5, // recent turns
		MaxSummaryTokens:       512,
		SnipThreshold:          0.70, // cheap tool-result trim, kept earlier than Compact
		SnipMaxToolResultChars: 800,
		MicrocompactMinChars:   4000, // disk-cache only genuinely big results
		CollapseThreshold:      0.78, // between snip(0.70) and compact(0.95)
		CollapseFoldWindow:     10,
		// MicrocompactDir is intentionally empty here — runtime sets it
		// per-session in setupRuntime so the cache lands under
		// ~/.metis/cache/<sessionID>/.

		// Protect long-term reference tools from Snip/Microcompact's
		// "[snipped: N chars]" rewrite. memory_query / memory_recall:
		// the model uses these for cross-session recall and frequently
		// re-cites the exact strings later. skill_help: typically a
		// short instruction sheet, shouldn't be dropped. Read: the
		// model is often actively planning around the read body, and
		// a stale "[snipped]" marker would force a redundant re-read
		// of the same file on the next iteration.
		ProtectedTools:        []string{"memory_query", "memory_recall", "skill_help", "Read"},
		RedactSecrets:         true,
		IterativeSummary:      true,
		MaxSummaryRetries:     2,
		IdleMaxSeconds:        3600, // 1h — mirrors CC's timeBasedMC cache-TTL window
		KeepRecentToolResults: 5,    // mirrors CC microCompact keepRecent=5
	}
}

// Compactor holds compaction state and executes summaries.
//
// MaxOutputTokens is the per-request `max_tokens` budget the provider
// will reserve for the assistant's reply. The Anthropic / OpenAI /
// MiniMax APIs all enforce `input_tokens + max_tokens <= context_window`,
// so the effective input cap is `MaxContextTokens - MaxOutputTokens`,
// not the full window. ShouldCompact accounts for this: with
// context_window=192k + max_tokens=64k, threshold should fire around
// `(192k - 64k) * 0.85 = 108k` input, not at 163k. Otherwise the user
// hits "context window exceeds limit" 4xx errors well before
// auto-compaction triggers (the 2026-04-30 user report against
// MiniMax-M2.7).
//
// MaxOutputTokens=0 falls back to using the full context window — keeps
// the test suite happy and matches the pre-bug behaviour for callers
// that don't know their per-request output budget.
type Compactor struct {
	Config
	Model            string
	MaxContextTokens int
	MaxOutputTokens  int
	Provider         llm.Provider

	// consecutiveFailures counts back-to-back Compact() failures (either
	// summarizer error or no-progress: cut adjustment swallowed the
	// middle). Reset to zero on a successful compaction. Used by the
	// circuit breaker — see MaxConsecutiveCompactFailures.
	//
	// Why we need this: claude-code observed 1279 sessions stuck in
	// compact-fail loops (max 3272 attempts in one session) wasting 250K
	// API calls/day globally. Without a circuit, a model that can't
	// produce a usable summary (rate-limited, OOM, gateway proxy
	// stripping responses) keeps re-trying every iteration.
	consecutiveFailures int

	// LastSummary holds the body of the most recent successful
	// boundary summary (without the "[Earlier conversation
	// summarized: ... ]" wrapper). When IterativeSummary=true the
	// next Compact() call feeds this back to the summarizer as a
	// seed so the LLM merges new middle messages into the running
	// narrative instead of re-summarizing from scratch (mirrors
	// hermes-agent context_compressor.py 2026-04 iterative mode).
	//
	// Cleared by ResetCircuit (which fires on /clear). Survives
	// across Compact calls within a session.
	LastSummary string
}

// MaxConsecutiveCompactFailures is the count after which the compactor
// refuses to attempt further compactions until the loop is reset. Matches
// claude-code's MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES = 3.
const MaxConsecutiveCompactFailures = 3

// PostCompactTokenBudget is the soft target for the assembled-message
// estimate AFTER Compact returns. When the post-compact estimate
// exceeds this, the boundary-cap pass walks tail tool_results and
// truncates them to PostCompactMaxToolResultChars to free room.
//
// Mirrors claude-code's POST_COMPACT_TOKEN_BUDGET = 50_000 — the
// thinking is that summarization should leave room for the next 1-2
// turns of new tool calls before triggering ANOTHER compaction. If
// the post-compact state already eats the budget, we'd ping-pong:
// compact → fail-overflow → recover → compact → fail again.
//
// Calibrated for MiniMax (192k context, 64k max_tokens → 128k input
// cap). 50k of post-compact baseline leaves 78k for the next 2-3
// turns. For Anthropic's 200k window the same constant is generous,
// not tight.
const PostCompactTokenBudget = 50_000

// PostCompactMaxToolResultChars caps each retained tail tool_result.
// 5000 chars ≈ 1250 tokens — tracks claude-code's
// POST_COMPACT_MAX_TOKENS_PER_FILE = 5_000. The model still sees the
// summary + first 5K of each tool's output (enough for "what was the
// result of grep X" style continuity) but a 50MB grep dump can't
// re-overflow the next request.
const PostCompactMaxToolResultChars = 5_000

func NewCompactor(cfg Config, model string, maxCtx int, p llm.Provider) *Compactor {
	if cfg.Threshold == 0 {
		cfg = DefaultCompactionConfig()
	}
	return &Compactor{Config: cfg, Model: model, MaxContextTokens: maxCtx, Provider: p}
}

// MaxReservedForSummary caps how many tokens metis reserves for
// the assistant's reply when computing effectiveInputCap. Pre-2026-
// 05-13 metis subtracted the FULL configured max_tokens from the
// context window — for users with max_tokens=64000, that ate 32%
// of a 200k window before compaction math even started, and
// auto-compact fired at ~54% of real window. claude-code caps
// the reservation at 20k (based on their p99.99 compact-summary
// output observation: services/compact/autoCompact.ts:30 → 20_000)
// because the typical reply is far smaller than the configured max.
//
// Mirror that here. Users wanting the legacy behavior (no compact
// before 0.85*(window-max_tokens)) can set
// `METIS_COMPACT_RESERVE_FULL_MAX_TOKENS=1` in env.
const MaxReservedForSummary = 20_000

// effectiveInputCap returns the input-only budget the threshold should
// be applied against. By default reserves min(MaxOutputTokens, 20k)
// for the assistant reply — the 20k cap stops a generous
// max_tokens config from prematurely shrinking the compactor's
// denominator. Equal to MaxContextTokens when MaxOutputTokens is 0
// (legacy / test path).
func (c *Compactor) effectiveInputCap() int {
	reserved := c.MaxOutputTokens
	if reserved > MaxReservedForSummary && os.Getenv("METIS_COMPACT_RESERVE_FULL_MAX_TOKENS") != "1" {
		reserved = MaxReservedForSummary
	}
	cap := c.MaxContextTokens - reserved
	if cap <= 0 {
		return c.MaxContextTokens
	}
	return cap
}

// ShouldCompact returns true when full summarization compaction should
// be triggered. Returns false when the circuit breaker has tripped (too
// many recent failures); callers must call ResetCircuit() — typically
// from /clear or /compact-reset — to re-enable.
//
// Also returns false when the estimated token count is below
// MinimumTokens — DeepSeek-TUI-style absolute floor that prevents
// rewriting prefix-cache anchors over a small session just because the
// user-configured max_tokens is low. MinimumTokens=0 disables this guard.
func (c *Compactor) ShouldCompact(messages []llm.Message) bool {
	if c.consecutiveFailures >= MaxConsecutiveCompactFailures {
		return false
	}
	rough := estimateTokens(messages)
	if c.MinimumTokens > 0 && rough < c.MinimumTokens {
		return false
	}
	return float64(rough) >= float64(c.effectiveInputCap())*c.Threshold
}

// ShouldSnip returns true when the cheap tool-result truncation pass
// should run. Triggers earlier than ShouldCompact so a tool-heavy
// session gets the dirt-cheap (no-LLM-call) cleanup BEFORE the
// expensive summarizer fires. Returns false if SnipThreshold is 0
// (snip disabled) or if the cheaper threshold isn't crossed yet.
func (c *Compactor) ShouldSnip(messages []llm.Message) bool {
	if c.SnipThreshold <= 0 {
		return false
	}
	rough := estimateTokens(messages)
	return float64(rough) >= float64(c.effectiveInputCap())*c.SnipThreshold
}

// ShouldCollapse reports whether the mid-conversation summary should
// run. Triggers between snip and full compact: lighter than full
// (only summarizes the EARLY middle, not most of the conversation),
// heavier than snip (involves an LLM call).
func (c *Compactor) ShouldCollapse(messages []llm.Message) bool {
	if c.consecutiveFailures >= MaxConsecutiveCompactFailures {
		return false
	}
	if c.CollapseThreshold <= 0 || c.CollapseFoldWindow <= 0 {
		return false
	}
	// Need ProtectFirst kept + at least CollapseFoldWindow to fold +
	// at least 1 message kept after the fold (otherwise the result is
	// just keepFirst + boundary, which is what full Compact does).
	if len(messages) < c.ProtectFirst+c.CollapseFoldWindow+1 {
		return false
	}
	rough := estimateTokens(messages)
	return float64(rough) >= float64(c.effectiveInputCap())*c.CollapseThreshold
}

// CollapseMiddle replaces messages [ProtectFirst : ProtectFirst+
// CollapseFoldWindow] with a single summary boundary message, then
// keeps everything from ProtectFirst+CollapseFoldWindow onward intact.
//
// Differs from Compact: Compact summarizes most of the conversation
// (everything except ProtectFirst + ProtectLast). Collapse only folds
// the EARLY middle — useful when the user has been on a long single
// thread and the early back-and-forth is no longer load-bearing but
// the full RECENT history still matters for in-flight reasoning.
//
// Falls back to no-op if tool-pair safety can't be maintained at the
// fold boundary (the kept tail must not start with an orphaned
// tool_result whose tool_use lives in the folded region).
func (c *Compactor) CollapseMiddle(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
	if c.CircuitTripped() {
		return messages, nil
	}
	if c.CollapseFoldWindow <= 0 {
		return messages, nil
	}
	if len(messages) < c.ProtectFirst+c.CollapseFoldWindow+1 {
		return messages, nil
	}
	end := c.ProtectFirst + c.CollapseFoldWindow
	end = adjustCutForToolPairs(messages, end)
	if end <= c.ProtectFirst {
		c.recordCompactResult(false, nil)
		return messages, nil
	}
	keepFirst := messages[:c.ProtectFirst]
	middle := messages[c.ProtectFirst:end]
	keepLater := messages[end:]

	// Collapse runs cold (no priorSummary seed) by design: it folds
	// EARLY-conversation context, not the running narrative, so an
	// iterative merge with c.LastSummary (which describes the
	// recent middle) would conflate two different scopes.
	summary, err := c.summarize(ctx, middle, "")
	if err != nil {
		c.recordCompactResult(false, err)
		return nil, err
	}
	boundary := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{{
			Type: "text",
			Text: "[Early conversation collapsed: " + summary + "]",
		}},
	}
	out := make([]llm.Message, 0, c.ProtectFirst+2+len(keepLater))
	out = append(out, keepFirst...)
	out = append(out, boundary)
	if len(keepLater) > 0 && keepLater[0].Role == llm.RoleAssistant {
		out = append(out, llm.Message{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Type: "text",
				Text: "(continuing from the collapsed early context above)",
			}},
		})
	}
	out = append(out, keepLater...)
	c.recordCompactResult(true, nil)
	return out, nil
}

// ShouldMicrocompact reports whether the disk-cache pass should run.
// Triggers in the same window as Snip but only fires when the disk
// cache is configured (MicrocompactDir non-empty). Caller invokes
// AFTER Snip — Snip handles the cheap cases; Microcompact catches
// the still-too-large blocks that snipping shouldn't touch (because
// the model might want them recovered later).
func (c *Compactor) ShouldMicrocompact(messages []llm.Message) bool {
	if c.MicrocompactDir == "" || c.MicrocompactMinChars <= 0 {
		return false
	}
	if c.SnipThreshold <= 0 {
		return false
	}
	rough := estimateTokens(messages)
	return float64(rough) >= float64(c.effectiveInputCap())*c.SnipThreshold
}

// toolResultPos locates a single tool_result block by (message index,
// content block index). Used as a map key by Microcompact's CC-mode
// keepRecent guard.
type toolResultPos struct {
	msgIdx, blockIdx int
}

// recentToolResultsToKeep walks the eligible range [ProtectFirst, cut)
// and returns the positions of the c.KeepRecentToolResults most recent
// tool_result blocks. The returned map is nil (empty lookup) when the
// guard is disabled (KeepRecentToolResults <= 0) — callers don't need
// to nil-check, missing key just returns the zero value.
//
// Walks both axes: the i index AND the bi index within Content matter
// because a single user-role message can contain multiple tool_results
// (parallel tool execution), and "most recent" should respect block
// order within that message.
func (c *Compactor) recentToolResultsToKeep(messages []llm.Message, cut int) map[toolResultPos]bool {
	if c.KeepRecentToolResults <= 0 {
		return nil
	}
	var positions []toolResultPos
	for i := c.ProtectFirst; i < cut; i++ {
		for bi, b := range messages[i].Content {
			if b.Type == "tool_result" {
				positions = append(positions, toolResultPos{i, bi})
			}
		}
	}
	start := len(positions) - c.KeepRecentToolResults
	if start < 0 {
		start = 0
	}
	keep := make(map[toolResultPos]bool, len(positions)-start)
	for _, p := range positions[start:] {
		keep[p] = true
	}
	return keep
}

// Microcompact off-loads oversized tool_result blocks (>= MicrocompactMinChars)
// to disk under MicrocompactDir/<tool_use_id>.txt. The inline content
// is replaced with a stub:
//
//	[output cached at PATH — N bytes; use Read tool with this exact path
//	 to recover full content]
//
// The model can re-fetch any cached output by calling Read on the
// path, which keeps Microcompact lossless from the model's POV (vs
// Snip which is permanently lossy). The protected tail is left intact.
func (c *Compactor) Microcompact(messages []llm.Message) []llm.Message {
	if c.MicrocompactDir == "" || c.MicrocompactMinChars <= 0 {
		return messages
	}
	cut := len(messages) - c.ProtectLast
	if cut <= c.ProtectFirst {
		return messages
	}
	if err := os.MkdirAll(c.MicrocompactDir, 0o700); err != nil {
		return messages // can't write cache → skip silently
	}
	idToName := toolNameByID(messages)
	protected := protectedSet(c.ProtectedTools)
	// 2026-05-15 CC-mode: mirror microCompact.ts keepRecent=5. In addition
	// to the trailing ProtectLast messages, never offload the N most
	// recent tool_result blocks in the eligible range. Without this, the
	// "model just made 5 Bash calls then user said '继续'" pattern would
	// silently evict all 5 results because the trailing 5 messages are
	// short user/assistant chatter.
	recentKeep := c.recentToolResultsToKeep(messages, cut)
	out := make([]llm.Message, len(messages))
	copy(out, messages)
	for i := c.ProtectFirst; i < cut; i++ {
		newContent := make([]llm.ContentBlock, len(out[i].Content))
		copy(newContent, out[i].Content)
		mutated := false
		for bi := range newContent {
			b := &newContent[bi]
			if b.Type != "tool_result" {
				continue
			}
			if len(b.ToolResult) < c.MicrocompactMinChars {
				continue
			}
			// Microcompact swaps the inline content for a "[output
			// cached at PATH]" stub — recoverable via Read, but only
			// if the model thinks to re-fetch it. For protected
			// tools (memory recall etc) the model usually doesn't
			// re-Read; the value is anchored in the next turn's
			// reasoning. Skip them.
			if isProtectedToolResult(b.ToolUseID, idToName, protected) {
				continue
			}
			// CC keepRecent guard — the most-recent N tool_results are
			// the model's working evidence, not cold history.
			if recentKeep[toolResultPos{i, bi}] {
				continue
			}
			id := b.ToolUseID
			if id == "" {
				id = fmt.Sprintf("anon_%d_%d", i, bi) // shouldn't happen but be defensive
			}
			path := filepath.Join(c.MicrocompactDir, id+".txt")
			if err := os.WriteFile(path, []byte(b.ToolResult), 0o600); err != nil {
				continue
			}
			b.ToolResult = fmt.Sprintf(
				"[output cached at %s — %d bytes; use Read tool with this exact path to recover full content]",
				path, len(b.ToolResult),
			)
			mutated = true
		}
		if mutated {
			out[i].Content = newContent
		}
	}
	return out
}

// Snip truncates oversized tool_result blocks in messages older than
// the protected tail. Cheap, lossy, and reversible-from-disk: the
// session.jsonl on disk still has the full output, only the in-memory
// transcript sent to the LLM is shrunk. Mirrors claude-code's "snip"
// tier from services/compact/.
//
// Why we keep the FIRST 200 chars and the marker (rather than dropping
// content entirely): the model often needs to know WHICH file/grep
// matched to plan the next step, but rarely needs to re-read the full
// dump. 200 chars is enough for the usual "found 14 matches in foo.go,
// bar.go, ..." preamble — and the [snipped: N chars] marker tells the
// model the truth so it doesn't hallucinate "all results visible".
//
// The protected tail (ProtectLast messages) is left intact: the agent
// often needs to re-read its most recent tool result to decide what to
// do next. Snip only touches older tool_results that have already
// served their purpose.
func (c *Compactor) Snip(messages []llm.Message) []llm.Message {
	if c.SnipMaxToolResultChars <= 0 {
		return messages
	}
	cut := len(messages) - c.ProtectLast
	if cut <= c.ProtectFirst {
		return messages // tail covers everything; nothing safe to snip
	}
	idToName := toolNameByID(messages)
	protected := protectedSet(c.ProtectedTools)
	out := make([]llm.Message, len(messages))
	copy(out, messages)

	// Walk only the snippable region. Mutate via a per-message Content
	// rebuild so the protected slices in the original `messages` aren't
	// shared (callers may still hold references).
	for i := c.ProtectFirst; i < cut; i++ {
		newContent := make([]llm.ContentBlock, len(out[i].Content))
		copy(newContent, out[i].Content)
		mutated := false
		for bi := range newContent {
			b := &newContent[bi]
			if b.Type != "tool_result" {
				continue
			}
			if len(b.ToolResult) <= c.SnipMaxToolResultChars {
				continue
			}
			// Idempotency guard: a tool_result that ALREADY carries a
			// "[snipped: N chars omitted]" marker has already passed
			// through Snip on a prior turn — re-snipping it would just
			// nibble the marker tail off (the bug caught on
			// 2026-05-08 by TestSnipE2E_RepeatedSnipIsIdempotent:
			// 1500 → 230 → 228 → 226 …). Skip when the marker is
			// present.
			if strings.Contains(b.ToolResult, "[snipped:") {
				continue
			}
			// Protected-tool whitelist: tool_results from tools like
			// memory_query / skill_help / Read are evidence the model
			// later cites; replacing them with "[snipped: N chars]"
			// silently breaks recall. Look up the matching tool_use
			// name by ID and skip when it's in ProtectedTools.
			if isProtectedToolResult(b.ToolUseID, idToName, protected) {
				continue
			}
			// Keep the first chunk (typically a "Found N matches" or
			// command-output preamble) plus a truthful marker so the
			// model knows content was dropped.
			head := b.ToolResult[:c.SnipMaxToolResultChars]
			omitted := len(b.ToolResult) - c.SnipMaxToolResultChars
			b.ToolResult = head + fmt.Sprintf("\n[snipped: %d chars omitted]", omitted)
			mutated = true
		}
		if mutated {
			out[i].Content = newContent
		}
	}
	return out
}

// EnforcePostCompactBudget walks every tool_result in messages and
// truncates anything longer than PostCompactMaxToolResultChars to
// that cap. Returns the same slice when nothing was over the cap.
//
// Different from Snip/SnipAll in two ways:
//  1. It uses the POST-COMPACT cap (5000 chars), not the
//     turn-time SnipMaxToolResultChars cap (typically 800).
//  2. It runs AFTER Compact's summarization, so the marker text
//     is "[truncated post-compact]" not "[snipped]" — the latter
//     would collide with Snip's idempotency guard and leave
//     tail dumps untouched on later snip passes.
//
// The cap matters for the post-compact request budget: even after
// 9-message compact, a single 10MB tail tool_result re-bloats the
// request and re-triggers MiniMax 2013. claude-code solves this
// with a per-attachment budget; metis takes the simpler approach
// of capping every retained tool_result at the same hard limit.
func (c *Compactor) EnforcePostCompactBudget(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, len(messages))
	copy(out, messages)
	mutatedAny := false
	for i := range out {
		newContent := make([]llm.ContentBlock, len(out[i].Content))
		copy(newContent, out[i].Content)
		mutatedRow := false
		for bi := range newContent {
			b := &newContent[bi]
			if b.Type != "tool_result" {
				continue
			}
			if len(b.ToolResult) <= PostCompactMaxToolResultChars {
				continue
			}
			if strings.Contains(b.ToolResult, "[truncated post-compact:") {
				continue
			}
			head := b.ToolResult[:PostCompactMaxToolResultChars]
			omitted := len(b.ToolResult) - PostCompactMaxToolResultChars
			b.ToolResult = head + fmt.Sprintf("\n[truncated post-compact: %d chars omitted]", omitted)
			mutatedRow = true
		}
		if mutatedRow {
			out[i].Content = newContent
			mutatedAny = true
		}
	}
	if !mutatedAny {
		return messages
	}
	return out
}

// SnipAll is Snip without the protected-tail exemption. Used by
// tryRecoverOverflow as a last resort: when the request already
// bounced with overflow, the protected-tail invariant doesn't matter
// (the model isn't going to read those tool_results next turn — the
// turn won't even leave the wire). Snipping the tail too can rescue
// requests where the OOM was caused by a tail tool dump that ordinary
// Snip would have spared.
//
// Same idempotency guard as Snip — already-snipped blocks aren't
// re-snipped.
func (c *Compactor) SnipAll(messages []llm.Message) []llm.Message {
	if c.SnipMaxToolResultChars <= 0 {
		return messages
	}
	idToName := toolNameByID(messages)
	protected := protectedSet(c.ProtectedTools)
	out := make([]llm.Message, len(messages))
	copy(out, messages)
	for i := c.ProtectFirst; i < len(out); i++ {
		newContent := make([]llm.ContentBlock, len(out[i].Content))
		copy(newContent, out[i].Content)
		mutated := false
		for bi := range newContent {
			b := &newContent[bi]
			if b.Type != "tool_result" {
				continue
			}
			if len(b.ToolResult) <= c.SnipMaxToolResultChars {
				continue
			}
			if strings.Contains(b.ToolResult, "[snipped:") {
				continue
			}
			// SnipAll runs as a last-ditch overflow rescue, so it's
			// MORE aggressive than Snip (touches the protected tail
			// too). It still respects ProtectedTools though — losing
			// a memory_query result during a panic rescue causes
			// permanent recall failure even if the bounce was
			// resolved, so the protection is structural not
			// performance.
			if isProtectedToolResult(b.ToolUseID, idToName, protected) {
				continue
			}
			head := b.ToolResult[:c.SnipMaxToolResultChars]
			omitted := len(b.ToolResult) - c.SnipMaxToolResultChars
			b.ToolResult = head + fmt.Sprintf("\n[snipped: %d chars omitted]", omitted)
			mutated = true
		}
		if mutated {
			out[i].Content = newContent
		}
	}
	return out
}

// CircuitTripped reports whether the breaker has opened. Callers (the
// Loop's compaction-check + the TUI status bar) use this to surface a
// "compaction disabled — N failures" notice instead of silently letting
// the context grow.
func (c *Compactor) CircuitTripped() bool {
	return c.consecutiveFailures >= MaxConsecutiveCompactFailures
}

// ResetCircuit zeroes the failure counter so the next ShouldCompact +
// Compact attempt is allowed. Wire this up to /clear and to any
// explicit "retry compaction" command. Also drops LastSummary so the
// next compaction starts cold rather than merging into a summary that
// the user just explicitly asked to throw away.
func (c *Compactor) ResetCircuit() {
	c.consecutiveFailures = 0
	c.LastSummary = ""
}

// recordCompactResult updates the failure counter. Called from Compact()
// just before returning so both error and no-progress outcomes are
// counted uniformly.
func (c *Compactor) recordCompactResult(progressed bool, err error) {
	if err == nil && progressed {
		c.consecutiveFailures = 0
		return
	}
	c.consecutiveFailures++
}

// Compact summarizes old messages into boundary turns.
// ProtectFirst and ProtectLast messages are kept intact.
//
// Tool-pair safety: the Anthropic Messages API rejects requests where a
// tool_result block has no matching tool_use earlier in the conversation
// (and vice versa). The cut point is adjusted so kept messages never start
// with an orphaned tool_result whose tool_use lives in the summarized middle.
func (c *Compactor) Compact(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
	// Circuit-breaker short-circuit: if the breaker is open, refuse to
	// even try. Caller should have already checked ShouldCompact, but
	// tryRecoverOverflow bypasses ShouldCompact, so we must guard here
	// too. The "no-op return" path doesn't count as a failure.
	if c.CircuitTripped() {
		return messages, nil
	}
	if len(messages) <= c.ProtectFirst+c.ProtectLast+2 {
		return messages, nil // nothing to compact
	}

	cut := len(messages) - c.ProtectLast
	cut = adjustCutForToolPairs(messages, cut)

	// Active-task anchor (2026-05-13 bugfix):
	//
	// ProtectFirst=1 keeps msgs[0] — almost always a greeting like
	// "你好" / "hi". ProtectLast=5 typically captures recent tool-result
	// + assistant cycles. The user's ACTUAL question — the one driving
	// the current turn — can land in the middle and get folded into the
	// assistant-role summary boundary. The model then scans the
	// post-compact slice, sees only ONE user-text message (the
	// greeting), and answers THAT instead of the active task.
	//
	// Real transcript that surfaced this (compaction inside a "compare
	// token consumption" turn):
	//   ...
	//   Conversation compacted (context compacted: 31 → 8 messages)
	//   ...
	//   The user just said "你好" (hello in Chinese). They previously
	//   had a task about comparing token consumption... 你好！
	//
	// Narrow fix: only pull cut earlier when the *natural* keepLast
	// has NO user-text message at all (purely tool cycles or assistant
	// chatter). In that pathological case, walk back to find the most
	// recent user-text message and pull cut earlier so that message
	// lands in keepLast verbatim. When keepLast already has a real
	// user prompt, no intervention — the existing slicing is fine.
	if !sliceHasUserText(messages[cut:]) {
		if anchor := lastUserTextBefore(messages, cut); anchor >= 0 && anchor < cut {
			cut = anchor
			cut = adjustCutForToolPairs(messages, cut)
		}
	}

	if cut <= c.ProtectFirst {
		// Tool-pair adjustment swallowed the middle; skip compaction.
		// Counts as a failure for the circuit because the conversation
		// shape is preventing progress — repeated calls won't help.
		c.recordCompactResult(false, nil)
		return messages, nil
	}

	keepFirst := messages[:c.ProtectFirst]
	middle := messages[c.ProtectFirst:cut]
	keepLast := messages[cut:]

	// Iterative mode: when middle[0] is the boundary message from a
	// PREVIOUS Compact (recognized by "[Earlier conversation
	// summarized: ... ]"), peel it off and seed summarize() with its
	// body so the LLM updates the running narrative instead of
	// re-summarizing from scratch. Falls through to non-iterative
	// when no prior summary exists in middle (first-time compact).
	priorSummary := c.LastSummary
	if len(middle) > 0 {
		if body, ok := extractPriorSummary(middle[0]); ok {
			priorSummary = body
			middle = middle[1:]
		}
	}

	summary, err := c.summarize(ctx, middle, priorSummary)
	if err != nil {
		c.recordCompactResult(false, err)
		return nil, err
	}
	c.LastSummary = summary

	// Boundary messages must use user / assistant role — Anthropic and
	// most compat gateways (notably MiniMax) reject `system` role
	// inside the messages array, error 2013: "chat content has invalid
	// message role: system". The system position is the top-level
	// `system` field, not a per-message slot.
	//
	// We emit as ASSISTANT — narratively reads like "I (the agent)
	// summarized our earlier conversation"; preserves user→assistant
	// alternation when keepLast[0] is the typical user message after
	// the cut. If keepLast happens to start with assistant (rare,
	// e.g. cut landed on an asst turn), prepend a synthetic user ack
	// to keep the strict alternation Anthropic / MiniMax enforce.
	boundary := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{{
			Type: "text",
			Text: "[Earlier conversation summarized: " + summary + "]",
		}},
	}

	out := make([]llm.Message, 0, c.ProtectFirst+3+len(keepLast))
	out = append(out, keepFirst...)
	out = append(out, boundary)
	// Post-compact attachment: surface the file paths the agent
	// touched in the SUMMARIZED middle. The summary text often loses
	// concrete paths ("worked on the auth module"); this hint gives
	// the model an explicit list so the next turn can Read the right
	// files without probing. Built from the FULL pre-compact slice so
	// even paths that were summarized away still register. Empty-zero
	// is "no Read/Edit/Write happened in scope" — common for
	// conversation-only sessions, skip the synthetic block then.
	if att := BuildPostCompactAttachment(messages); len(att.Content) > 0 {
		out = append(out, att)
	} else if len(keepLast) > 0 && keepLast[0].Role == llm.RoleAssistant {
		// No attachment to bridge with — but we still need a user ack
		// when keepLast starts with assistant, since two consecutive
		// assistant messages would be rejected by Anthropic / MiniMax.
		out = append(out, llm.Message{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Type: "text",
				Text: "(continuing from the summarized context above)",
			}},
		})
	}
	out = append(out, keepLast...)
	c.recordCompactResult(true, nil)
	return out, nil
}

// adjustCutForToolPairs walks the cut point earlier so the kept tail never
// starts with a tool_result whose matching tool_use is in the discarded
// middle. Caller checks the result against ProtectFirst to decide whether
// compaction is still possible.
func adjustCutForToolPairs(messages []llm.Message, cut int) int {
	for cut > 0 && cut < len(messages) && messageHasToolResult(messages[cut]) {
		cut--
	}
	return cut
}

func messageHasToolResult(m llm.Message) bool {
	for _, b := range m.Content {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

// toolNameByID builds an index from tool_use_id → tool name across all
// messages. Used by Snip / SnipAll / Microcompact to look up a
// tool_result's originating tool (the `tool_result` block itself only
// carries the ID; the name lives on the matching `tool_use` block).
// Returns empty map when no tool_use blocks are present.
func toolNameByID(messages []llm.Message) map[string]string {
	out := make(map[string]string)
	for _, m := range messages {
		for _, b := range m.Content {
			if b.Type == "tool_use" && b.ToolUseID != "" && b.ToolName != "" {
				out[b.ToolUseID] = b.ToolName
			}
		}
	}
	return out
}

// protectedSet builds a case-insensitive set from the configured
// ProtectedTools slice. Centralizing the casefold so the caller can
// pass tool names verbatim in config (`Read`, `memory_query`) without
// worrying about how a particular provider's adapter normalizes them.
func protectedSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[strings.ToLower(n)] = struct{}{}
	}
	return out
}

// isProtectedToolResult reports whether the tool_result identified by
// `id` came from a tool in the protected set. Returns false when the
// set is empty (legacy / disabled) or the matching tool_use isn't
// found (defensive: protecting a stranded tool_result by name we
// can't resolve would silently un-snip random blocks).
func isProtectedToolResult(id string, idToName map[string]string, protected map[string]struct{}) bool {
	if len(protected) == 0 || id == "" {
		return false
	}
	name, ok := idToName[id]
	if !ok {
		return false
	}
	_, prot := protected[strings.ToLower(name)]
	return prot
}

// sliceHasUserText reports whether at least one message in `slice` is
// USER-role AND carries a non-empty text block (not just tool_result
// echoes). Used by Compact's anchor logic to decide whether the natural
// keepLast already contains a user prompt — if so, no anchor pull
// needed.
func sliceHasUserText(slice []llm.Message) bool {
	for _, m := range slice {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				return true
			}
		}
	}
	return false
}

// lastUserTextBefore finds the index of the most recent USER-role
// message whose content has at least one text block, scanning
// backwards from `before` (exclusive). Returns -1 when no such
// message exists.
//
// "User text" specifically excludes user messages that are pure
// tool_result echoes — those aren't initiated by the human and
// would not anchor the model's "current task" reasoning.
//
// Used by Compact to ensure the user's active prompt survives the
// summarize-middle pass instead of getting folded into the
// assistant-role boundary (where the model reads it as "history",
// not "active task").
func lastUserTextBefore(messages []llm.Message, before int) int {
	if before > len(messages) {
		before = len(messages)
	}
	for i := before - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				return i
			}
		}
	}
	return -1
}

// SummarySystemPromptInitial is the system instruction the summarizer
// LLM sees on a FRESH compact (no prior summary to merge). The 5-section
// structure mirrors crush's internal/agent/templates/summary.md and is
// strong enough to survive across providers (Anthropic / GLM / Kimi /
// DeepSeek all handle markdown sections well; raw "summarize concisely"
// without structure tends to collapse to a single paragraph the model
// then mis-reads as "small talk recap").
const SummarySystemPromptInitial = `You are summarizing an agent conversation so it can be folded into a context-window boundary. The summary is the ONLY context the agent will have for what happened before this point — be specific, not glossy.

Output the summary as concise Markdown with these five sections in order. Omit a section only if it is genuinely empty.

## Current State
- Exact user request (verbatim if short, paraphrased if long, but include the actual ask)
- What's done / in progress / blocked
- What remains, with concrete next-step granularity

## Files & Changes
- Files written/edited (path + one-line purpose)
- Files read for context (path + why)
- Key code locations the agent is anchored on (file:line)

## Technical Context
- Architecture / pattern decisions and the reason
- Libraries, commands, environment details that worked or failed (exact form)
- Constants, IDs, paths, error messages worth preserving

## Strategy & Approach
- Overall approach and why it was chosen
- Known gotchas, assumptions, blockers
- Branches considered and rejected (so the agent doesn't re-explore)

## Exact Next Steps
- Numbered, imperative, concrete (e.g. "Add JWT middleware to src/middleware/auth.js:15")
- Include exact commands or test invocations the agent should run

Tone: briefing a teammate who just walked in cold. No emojis. No filler. No "the user wanted to know about X" framing — write what the actual answer was. Err on too much specificity rather than too little.`

// SummarySystemPromptMerge is the system instruction used when a prior
// summary already exists (iterative mode, hermes-agent style). The LLM
// is asked to UPDATE the existing summary with new middle messages
// rather than re-summarize from scratch. Priority on conflict:
//
//	Active Task > Completed Actions > Resolved Questions.
const SummarySystemPromptMerge = `You are UPDATING an existing conversation summary with new messages that have happened since the last summary. The previous summary will be shown to you; treat it as authoritative for everything that came before, and integrate the new messages into the same five-section structure.

Output the updated summary as concise Markdown using these sections:

## Current State
## Files & Changes
## Technical Context
## Strategy & Approach
## Exact Next Steps

Rules:
1. Promote items that have changed status (e.g. an Exact Next Step that's now Done moves to Current State as "completed: ...")
2. When new messages contradict the prior summary, the new messages win
3. When prior summary and new messages are about different things, KEEP BOTH — do not drop prior content just to make room
4. Drop trivia that has been clearly superseded; keep concrete file paths, IDs, error messages, exact commands
5. Priority on conflict: Active Task > Completed Actions > Resolved Questions

Tone: briefing a teammate cold. No emojis. No filler.`

// secretPatterns is the redaction regex set applied to message text
// before it's shipped to the summarizer. Order matters: longer / more
// specific patterns first so a generic "token: xxx" doesn't pre-empt
// "github_pat_xxx". Conservative — false positives are cheap (the
// summary just reads "[REDACTED]" instead of the real value); false
// negatives are not (a real key bakes into every future turn).
var secretPatterns = []*regexp.Regexp{
	// Anthropic / OpenAI / Stripe style: sk-... 24+ chars
	regexp.MustCompile(`sk[-_][A-Za-z0-9_\-]{20,}`),
	// GitHub PAT / fine-grained / OAuth
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	// AWS access key ID
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// JWT-ish triple-segment base64url
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`),
	// Generic "key: VALUE" / "token: VALUE" with hex/base64 value (≥16)
	regexp.MustCompile(`(?i)(api[_\-]?key|access[_\-]?token|secret[_\-]?key|password|bearer)\s*[:=]\s*['"]?([A-Za-z0-9_\-+/=]{16,})['"]?`),
}

// redactSecrets returns s with likely API keys / tokens / passwords
// replaced by `[REDACTED]`. Defensive; cheap to run. Idempotent — a
// string that's already had [REDACTED] inserted produces the same
// output.
func redactSecrets(s string) string {
	if s == "" {
		return s
	}
	for i, re := range secretPatterns {
		// The final pattern has two capture groups (key + value); keep
		// the key name in the output so the model still sees "api_key:
		// [REDACTED]" rather than just "[REDACTED]" floating alone.
		if i == len(secretPatterns)-1 {
			s = re.ReplaceAllString(s, "${1}: [REDACTED]")
		} else {
			s = re.ReplaceAllString(s, "[REDACTED]")
		}
	}
	return s
}

// summarize is the streaming-with-retry-with-non-stream-fallback path
// used by Compact. Returns a single string (the summary body); the
// caller wraps it in the boundary message.
//
// When IterativeSummary=true AND priorSummary != "", the LLM is asked
// to update the prior summary with the new middle messages; otherwise
// it summarizes from scratch with the 5-section template.
//
// Redaction (RedactSecrets=true) runs on each per-message text body
// just before it joins the prompt. Long messages are NOT individually
// length-capped here — the broader compaction tiering already snipped
// or microcompacted oversized tool_results before Compact called us.
//
// Retry policy: streaming attempts 1..(1+MaxSummaryRetries) with
// jittered backoff between them, then ONE non-streaming Complete()
// attempt as a last resort. The non-stream fallback exists because
// the most common streaming failure mode metis sees is "SSE channel
// drops mid-summary" (gateway proxies, MiniMax 2013-on-stream); a
// plain Complete request reliably gets through when the same prompt
// would have failed streaming.
func (c *Compactor) summarize(ctx context.Context, messages []llm.Message, priorSummary string) (string, error) {
	useIterative := c.IterativeSummary && strings.TrimSpace(priorSummary) != ""

	system := SummarySystemPromptInitial
	if useIterative {
		system = SummarySystemPromptMerge
	}

	var b strings.Builder
	if useIterative {
		b.WriteString("Previous summary (authoritative for everything before the new messages):\n\n")
		b.WriteString(maybeRedact(priorSummary, c.RedactSecrets))
		b.WriteString("\n\n---\n\nNew messages since that summary:\n\n")
	} else {
		b.WriteString("Conversation transcript to summarize:\n\n")
	}
	for _, m := range messages {
		role := strings.ToUpper(string(m.Role))
		for _, blk := range m.Content {
			switch blk.Type {
			case "text":
				if blk.Text == "" {
					continue
				}
				b.WriteString(role + ": " + maybeRedact(blk.Text, c.RedactSecrets) + "\n")
			case "tool_use":
				b.WriteString(role + " tool_use(" + blk.ToolName + ")\n")
			case "tool_result":
				// Include a TRIMMED tool_result peek (first 400 chars)
				// — the summarizer needs to know what the tools
				// returned to write a useful Files & Changes section,
				// but doesn't need the full 5KB. Snip / microcompact
				// already shortened older results; this peek is
				// belt-and-braces for any that slipped through.
				peek := blk.ToolResult
				if len(peek) > 400 {
					peek = peek[:400] + "...[truncated for summary]"
				}
				if peek != "" {
					b.WriteString(role + " tool_result: " + maybeRedact(peek, c.RedactSecrets) + "\n")
				}
			}
		}
	}
	if useIterative {
		b.WriteString("\nUpdated summary:")
	} else {
		b.WriteString("\nSummary:")
	}

	req := llm.Request{
		Model:     c.Model,
		System:    system,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: b.String()}}}},
		MaxTokens: c.MaxSummaryTokens,
	}

	maxRetries := c.MaxSummaryRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Jittered backoff: 50..200ms × attempt. Cheap by
			// design — we're not waiting on rate limits, just
			// giving a flaky gateway a moment to recover.
			d := time.Duration(50+rand.Intn(150)) * time.Millisecond * time.Duration(attempt)
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		out, err := c.summarizeOnce(ctx, req)
		if err == nil && strings.TrimSpace(out) != "" {
			return out, nil
		}
		if err != nil {
			lastErr = err
		}
	}

	// Final fallback: non-streaming Complete(). This handles the
	// common "streaming drops half-way but a one-shot request would
	// succeed" pattern observed against MiniMax + flaky reverse
	// proxies.
	resp, err := c.Provider.Complete(ctx, req)
	if err != nil {
		if lastErr != nil {
			return "", fmt.Errorf("summarize: stream+complete both failed: stream=%w complete=%v", lastErr, err)
		}
		return "", err
	}
	for _, blk := range resp.Content {
		if blk.Type == "text" && strings.TrimSpace(blk.Text) != "" {
			return strings.TrimSpace(blk.Text), nil
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("summarize: empty fallback response (prior stream err: %v)", lastErr)
	}
	return "", errors.New("summarize: empty response from both stream and Complete fallback")
}

// summarizeOnce runs a single streaming attempt against the provider
// and returns either a non-empty trimmed summary or an error. Empty
// streamed output is treated as a soft failure so the retry loop has
// something to react to.
func (c *Compactor) summarizeOnce(ctx context.Context, req llm.Request) (string, error) {
	stream, err := c.Provider.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	// Progress fan-out — when maybeCompact stamped a forwarder onto ctx
	// (via WithEventOut), every text_delta we receive bumps the byte
	// counter and emits an EventCompactionProgress. The TUI uses this
	// to render "Compacting conversation (12K tokens streamed)" so a
	// long summarize doesn't look like a freeze. Mirrors CC's
	// responseLengthRef-driven spinner counter. nil channel → silent
	// (the pre-2026-05-15 behaviour, and what tests get).
	progressCh := EventOutFromContext(ctx)
	// Don't emit on every single delta — a streaming summarize can fire
	// hundreds of tiny chunks/s. throttleEvery limits emission to one
	// event per ~256 bytes (≈64 tokens), which gives a visibly-moving
	// counter without flooding the UI message bus.
	const throttleEvery = 256
	var nextEmit int
	var out strings.Builder
	for {
		ev, err := stream.Recv()
		if err != nil {
			if errIsEOF(err) {
				break
			}
			return "", err
		}
		switch ev.Type {
		case "text_delta":
			out.WriteString(ev.TextDelta)
			if progressCh != nil && out.Len() >= nextEmit {
				select {
				case progressCh <- Event{Kind: EventCompactionProgress, InputTokens: out.Len()}:
				default:
					// Drop on full channel — the next delta's emit will
					// catch up. Better than blocking summarize on a slow
					// UI consumer.
				}
				nextEmit = out.Len() + throttleEvery
			}
		case "message_stop":
			return strings.TrimSpace(out.String()), nil
		case "error":
			if ev.Err != nil {
				return "", ev.Err
			}
		}
	}
	return strings.TrimSpace(out.String()), nil
}

// maybeRedact applies redactSecrets only when the gate is on.
// Splitting this out keeps the per-message loop readable and lets
// tests pin the redaction-OFF path without rebuilding the regex set.
func maybeRedact(s string, on bool) string {
	if !on {
		return s
	}
	return redactSecrets(s)
}

// extractPriorSummary returns (body, true) when m is the boundary
// summary message produced by a previous Compact (recognized by the
// "[Earlier conversation summarized: ... ]" wrapper). Used by Compact
// to peel off the prior summary BEFORE passing the new middle to
// summarize() — so the LLM doesn't see "Earlier conversation
// summarized: [body]" as a regular message in addition to receiving
// body via the iterative seed.
func extractPriorSummary(m llm.Message) (string, bool) {
	if m.Role != llm.RoleAssistant {
		return "", false
	}
	for _, b := range m.Content {
		if b.Type != "text" {
			continue
		}
		const prefix = "[Earlier conversation summarized: "
		const suffix = "]"
		t := b.Text
		if !strings.HasPrefix(t, prefix) || !strings.HasSuffix(t, suffix) {
			continue
		}
		body := strings.TrimSuffix(strings.TrimPrefix(t, prefix), suffix)
		return body, true
	}
	return "", false
}

// estimateTokens is a rough token estimator.
//
// Counts everything that gets serialized to the wire: text bodies, tool_use
// inputs (the JSON arg blob), and tool_result content. Earlier the estimator
// only summed Text — which left tool-heavy turns invisible to ShouldCompact
// and let the budget overflow before compaction fired. The user hit
// "context window exceeds limit (2013)" because of this gap.
//
// Adaptive per-string ratio (file-aware + CJK-aware, mirrors
// claude-code's tokenEstimation.ts file-type heuristic):
//
//   - Plain English / mostly ASCII text:   chars / 4   (~ 4 chars per token)
//   - Mostly CJK (中文 / 日本 / 한글):       chars / 1   (1 token per char)
//   - JSON-heavy (>=20% braces/quotes):    chars / 2.5 (denser)
//
// Single-pass scanner per string so the per-block hot path stays cheap.
// All ratios round to integers — sub-token-per-char fractions are below
// the noise floor for context-window thresholds.
func estimateTokens(messages []llm.Message) int {
	total := 0
	for _, m := range messages {
		total += 4 // per-message role/wrapper overhead (~16 chars JSON)
		for _, c := range m.Content {
			total += estimateContentBlockTokens(c)
		}
	}
	return total
}

// estimateContentBlockTokens budgets one ContentBlock for the
// estimator. Extracted from estimateTokens so the recursive
// tool_result.ToolResultBlocks case can reuse the per-block
// accounting (image-bearing tool_result blocks are otherwise
// invisible to the size estimator — pre-2026-05-27 bug surfaced as
// Kimi sessions blowing 262k cap despite our local estimator
// saying ~80k).
func estimateContentBlockTokens(c llm.ContentBlock) int {
	total := 8 // per-block JSON envelope: {"type":"...",...} ~ 8 tokens
	total += estimateStringTokens(c.Text)
	total += estimateStringTokens(c.ToolResult)
	total += len(c.ToolName) / 4
	total += len(c.ToolUseID) / 4
	for k, v := range c.ToolInput {
		total += len(k) / 4
		total += approxValueLen(v) / 4
	}
	// Image data: base64 is near-1:1 against the model tokeniser
	// (high-entropy alphabet, no whitespace, no language-model-
	// friendly prefixes). 4 chars/token underestimates by ~3x on
	// the server side, which is what made Kimi sessions silently
	// blow through the 262k cap. The 0.75 multiplier puts the
	// estimate closer to the actual tiktoken-cl100k cost.
	if c.Type == "image" && len(c.Data) > 0 {
		total += int(float64(len(c.Data)) * 0.75)
	}
	// tool_result with vision-aware sub-blocks (ViewImage,
	// cu screenshot-attaching tools): recurse so the inner
	// image counts toward the parent message.
	if len(c.ToolResultBlocks) > 0 {
		for _, sub := range c.ToolResultBlocks {
			total += estimateContentBlockTokens(sub)
		}
	}
	return total
}

// estimateStringTokens returns a content-aware token estimate for a
// single string. Splits into three regimes:
//
//	CJK-dominant (≥50% CJK runes):    1 token per char
//	JSON-dominant (≥20% {}",: chars): 1 token per ~2.5 chars
//	default ASCII-ish text:           1 token per 4 chars
//
// Returns 0 for empty input. Cost is O(len(s)) one-pass — counts
// the three regime indicators in the same loop instead of three
// separate scans.
func estimateStringTokens(s string) int {
	if s == "" {
		return 0
	}
	var cjk, json, total int
	for _, r := range s {
		total++
		switch {
		case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r),
			unicode.Is(unicode.Katakana, r), unicode.Is(unicode.Hangul, r):
			cjk++
		case r == '{', r == '}', r == '[', r == ']', r == '"', r == ':', r == ',':
			json++
		}
	}
	if total == 0 {
		return 0
	}
	// CJK wins first — CJK + JSON braces around them (e.g. an i18n
	// translation file) should still bill CJK rates because CJK is
	// where the token density is.
	if cjk*2 >= total {
		// 1 token per char. byte-length / rune-count is irrelevant
		// here; provider tokenizers bill per CJK glyph.
		return cjk + (total-cjk)/4
	}
	if json*5 >= total {
		// 2.5 chars/token → multiply by 2 / divide by 5 keeps it
		// integer-only without floating-point.
		return (total * 2) / 5
	}
	return total / 4
}

// approxValueLen returns a rough byte-length for a JSON value without
// re-serializing it. Strings and numbers dominate real tool inputs;
// maps/slices recurse so a nested structure isn't free.
func approxValueLen(v any) int {
	switch x := v.(type) {
	case string:
		return len(x) + 2 // quotes
	case bool:
		return 5
	case nil:
		return 4
	case float64, int, int64:
		return 8 // rough
	case map[string]any:
		n := 2 // braces
		for k, vv := range x {
			n += len(k) + 4 + approxValueLen(vv)
		}
		return n
	case []any:
		n := 2
		for _, vv := range x {
			n += approxValueLen(vv) + 1
		}
		return n
	default:
		return 16
	}
}

// Marshal for JSON serialization of compacted history.
func CompactState(messages []llm.Message) ([]byte, error) {
	return json.Marshal(messages)
}
