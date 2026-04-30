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
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// errIsEOF returns true for the io.EOF the streaming providers signal
// when the SSE channel closes cleanly. Kept inline so summarize stays
// readable; bigger compaction package isn't worth a util file.
func errIsEOF(err error) bool { return errors.Is(err, io.EOF) }

// Config holds compaction thresholds.
type Config struct {
	Threshold        float64 // fraction of context window (e.g. 0.85)
	ProtectFirst     int     // always keep first N messages
	ProtectLast      int     // always keep last N messages
	MaxSummaryTokens int     // max tokens in a summary turn
}

// DefaultConfig returns sensible defaults.
func DefaultCompactionConfig() Config {
	return Config{
		Threshold:        0.85,
		ProtectFirst:     1, // system message
		ProtectLast:      5, // recent turns
		MaxSummaryTokens: 512,
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
}

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

// ShouldCompact returns true when compaction should be triggered.
func (c *Compactor) ShouldCompact(messages []llm.Message) bool {
	rough := estimateTokens(messages)
	return float64(rough) >= float64(c.effectiveInputCap())*c.Threshold
}

// Compact summarizes old messages into boundary turns.
// ProtectFirst and ProtectLast messages are kept intact.
//
// Tool-pair safety: the Anthropic Messages API rejects requests where a
// tool_result block has no matching tool_use earlier in the conversation
// (and vice versa). The cut point is adjusted so kept messages never start
// with an orphaned tool_result whose tool_use lives in the summarized middle.
func (c *Compactor) Compact(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
	if len(messages) <= c.ProtectFirst+c.ProtectLast+2 {
		return messages, nil // nothing to compact
	}

	cut := len(messages) - c.ProtectLast
	cut = adjustCutForToolPairs(messages, cut)
	if cut <= c.ProtectFirst {
		// Tool-pair adjustment swallowed the middle; skip compaction.
		return messages, nil
	}

	keepFirst := messages[:c.ProtectFirst]
	middle := messages[c.ProtectFirst:cut]
	keepLast := messages[cut:]

	summary, err := c.summarize(ctx, middle)
	if err != nil {
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
