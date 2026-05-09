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
	"os"
	"path/filepath"
	"strings"

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
}

// DefaultConfig returns sensible defaults.
func DefaultCompactionConfig() Config {
	return Config{
		Threshold:              0.85,
		ProtectFirst:           1, // system message
		ProtectLast:            5, // recent turns
		MaxSummaryTokens:       512,
		SnipThreshold:          0.70, // ~15% earlier than full compact
		SnipMaxToolResultChars: 800,
		MicrocompactMinChars:   4000, // disk-cache only genuinely big results
		CollapseThreshold:      0.78, // between snip(0.70) and compact(0.85)
		CollapseFoldWindow:     10,
		// MicrocompactDir is intentionally empty here — runtime sets it
		// per-session in setupRuntime so the cache lands under
		// ~/.metis/cache/<sessionID>/.
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
}

// MaxConsecutiveCompactFailures is the count after which the compactor
// refuses to attempt further compactions until the loop is reset. Matches
// claude-code's MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES = 3.
const MaxConsecutiveCompactFailures = 3

func NewCompactor(cfg Config, model string, maxCtx int, p llm.Provider) *Compactor {
	if cfg.Threshold == 0 {
		cfg = DefaultCompactionConfig()
	}
	return &Compactor{Config: cfg, Model: model, MaxContextTokens: maxCtx, Provider: p}
}

// effectiveInputCap returns the input-only budget the threshold should
// be applied against. Equal to MaxContextTokens when MaxOutputTokens
// is 0 (legacy / test path).
func (c *Compactor) effectiveInputCap() int {
	cap := c.MaxContextTokens - c.MaxOutputTokens
	if cap <= 0 {
		return c.MaxContextTokens
	}
	return cap
}

// ShouldCompact returns true when full summarization compaction should
// be triggered. Returns false when the circuit breaker has tripped (too
// many recent failures); callers must call ResetCircuit() — typically
// from /clear or /compact-reset — to re-enable.
func (c *Compactor) ShouldCompact(messages []llm.Message) bool {
	if c.consecutiveFailures >= MaxConsecutiveCompactFailures {
		return false
	}
	rough := estimateTokens(messages)
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

	summary, err := c.summarize(ctx, middle)
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

// CircuitTripped reports whether the breaker has opened. Callers (the
// Loop's compaction-check + the TUI status bar) use this to surface a
// "compaction disabled — N failures" notice instead of silently letting
// the context grow.
func (c *Compactor) CircuitTripped() bool {
	return c.consecutiveFailures >= MaxConsecutiveCompactFailures
}

// ResetCircuit zeroes the failure counter so the next ShouldCompact +
// Compact attempt is allowed. Wire this up to /clear and to any
// explicit "retry compaction" command.
func (c *Compactor) ResetCircuit() {
	c.consecutiveFailures = 0
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

	summary, err := c.summarize(ctx, middle)
	if err != nil {
		c.recordCompactResult(false, err)
		return nil, err
	}

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

	out := make([]llm.Message, 0, c.ProtectFirst+2+len(keepLast))
	out = append(out, keepFirst...)
	out = append(out, boundary)
	if len(keepLast) > 0 && keepLast[0].Role == llm.RoleAssistant {
		// Two consecutive assistant messages would be rejected; insert
		// a placeholder user ack between them. Cheap and never
		// surfaces in the UI (this is API plumbing).
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

func (c *Compactor) summarize(ctx context.Context, messages []llm.Message) (string, error) {
	// Build a summary prompt
	var b strings.Builder
	b.WriteString("Summarize the following conversation concisely (max ")
	b.WriteString(fmt.Sprintf("%d words", c.MaxSummaryTokens/2))
	b.WriteString("). Focus on: key decisions, facts, and any pending tasks.\n\n")
	for _, m := range messages {
		role := strings.ToUpper(string(m.Role))
		for _, c := range m.Content {
			if c.Type == "text" && c.Text != "" {
				b.WriteString(role + ": " + c.Text + "\n")
			}
		}
	}
	b.WriteString("\nSummary:")
	req := llm.Request{
		Model:     c.Model,
		System:    "You summarize conversations concisely.",
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: b.String()}}}},
		MaxTokens: c.MaxSummaryTokens,
	}
	// Stream the summary so the request profile matches the rest of the
	// turn loop (no synchronous Complete pause that the user feels as
	// "stuck"). The caller still ends up with a single string — we just
	// concat the deltas. If the provider's streaming path errors out we
	// don't fall back to Complete: rare enough, and a single Complete
	// call would re-introduce the very behaviour we're trying to remove.
	stream, err := c.Provider.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()
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

// estimateTokens is a rough token estimator (4 chars ≈ 1 token for English).
//
// Counts everything that gets serialized to the wire: text bodies, tool_use
// inputs (the JSON arg blob), and tool_result content. Earlier the estimator
// only summed Text — which left tool-heavy turns invisible to ShouldCompact
// and let the budget overflow before compaction fired. The user hit
// "context window exceeds limit (2013)" because of this gap.
func estimateTokens(messages []llm.Message) int {
	total := 0
	for _, m := range messages {
		total += 4 // per-message role/wrapper overhead (~16 chars JSON)
		for _, c := range m.Content {
			// Per-block JSON envelope: {"type":"...",...} ~ 8 tokens.
			total += 8
			total += len(c.Text) / 4
			total += len(c.ToolResult) / 4
			total += len(c.ToolName) / 4
			total += len(c.ToolUseID) / 4
			// Tool input — count the keys + naive value lengths.
			for k, v := range c.ToolInput {
				total += len(k) / 4
				total += approxValueLen(v) / 4
			}
		}
	}
	return total
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
