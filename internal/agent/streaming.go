package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/argsunwrap"
)

// usageTotals holds the input / output token tallies a streaming response
// emits across multiple message_delta events. Kept package-private; the
// final tally is forwarded to callers as an Event{Kind: EventTokens}.
type usageTotals struct {
	in, out                int
	cacheCreate, cacheRead int
}

// consumeStream reads StreamEvents from the LLM stream, emits text and tool
// events on `out`, and returns the assembled assistant message blocks plus
// the stop reason and final token counts.
//
// Lock-free with respect to l.mu — the LLM call doesn't need the loop's
// state lock; that simplifies reasoning about deadlocks since this is the
// longest-running phase of a turn.
func (l *Loop) consumeStream(ctx context.Context, s llm.StreamReader, out chan<- Event) ([]llm.ContentBlock, string, *usageTotals, error) {
	var (
		blocks      []llm.ContentBlock
		curText     string
		curThinking string
		stopReas    string
		usage       usageTotals
	)
	type streamedTool struct {
		blockIndex int
		json       string
		finished   bool
	}
	toolsByID := make(map[string]*streamedTool)
	var toolOrder []*streamedTool
	flushText := func() {
		if curText != "" {
			blocks = append(blocks, llm.ContentBlock{Type: "text", Text: curText})
			curText = ""
		}
	}
	// flushThinking persists the accumulated reasoning trace as a
	// ContentBlock on the assistant message. Earlier metis treated
	// thinking as a transient TUI-only trace (the flushed message had
	// no thinking blocks, so session jsonl + resume lost the reasoning
	// entirely). Mirrors crush's ReasoningContent and openclaude's
	// Anthropic ThinkingBlock — keep the trace alongside text/tool_use
	// so the user can re-read it after a /resume and so providers that
	// REQUIRE the thinking block to be re-sent (Anthropic with
	// extended-thinking enabled) get a faithful round-trip.
	//
	// Provider adapters that DON'T accept thinking blocks (DeepSeek,
	// GLM, MiniMax) strip them on the send path — see
	// internal/llm/openai/openai.go's request builder.
	flushThinking := func() {
		if curThinking != "" {
			// Thinking sits BEFORE the text/tool blocks it produced —
			// matches Anthropic's wire-format ordering and reads
			// chronologically when persisted.
			blocks = append(blocks, llm.ContentBlock{Type: "thinking", Text: curThinking})
			curThinking = ""
		}
	}
	flushTool := func(tool *streamedTool) {
		if tool == nil || tool.finished {
			return
		}
		tool.finished = true
		if tool.json != "" {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(tool.json), &parsed); err == nil {
				// MiniMax-M2.7 anthropic-shim wraps args as
				// {"_": "<json-object-string>"} (session 87e366fa);
				// argsunwrap is a no-op for every well-formed shape so
				// applying it unconditionally is safe across providers.
				// See internal/llm/argsunwrap/argsunwrap.go for the
				// structural guard.
				blocks[tool.blockIndex].ToolInput = argsunwrap.Unwrap(parsed)
			} else {
				blocks[tool.blockIndex].ToolInput = map[string]any{"_raw": tool.json}
			}
		}
	}
	flushTools := func() {
		for _, tool := range toolOrder {
			flushTool(tool)
		}
	}
	lookupTool := func(id string) *streamedTool {
		if tool := toolsByID[id]; tool != nil && !tool.finished {
			return tool
		}
		// A few compatibility providers omit the id on delta/stop events.
		// Preserve the historical single-tool behaviour when attribution is
		// unambiguous, but never guess when multiple calls are in flight.
		if id == "" {
			var only *streamedTool
			for _, tool := range toolsByID {
				if tool.finished {
					continue
				}
				if only != nil {
					return nil
				}
				only = tool
			}
			return only
		}
		return nil
	}
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			flushThinking()
			flushText()
			flushTools()
			return blocks, stopReas, &usage, nil
		}
		if err != nil {
			return nil, "", nil, err
		}
		switch ev.Type {
		case "message_start":
			usage.in = ev.InputTokens
			// Cache fields land at message_start on Anthropic native; some
			// gateways only emit them in message_delta. Capture either way.
			if ev.CacheCreationInputTokens > 0 {
				usage.cacheCreate = ev.CacheCreationInputTokens
			}
			if ev.CacheReadInputTokens > 0 {
				usage.cacheRead = ev.CacheReadInputTokens
			}
		case "text_delta":
			// Reasoning ends as soon as the assistant starts emitting
			// text — flush it as a block now so the order in `blocks`
			// is [thinking, text, ...] which is the order the model
			// produced + what Anthropic's wire format expects.
			flushThinking()
			curText += ev.TextDelta
			emit(ctx, out, Event{Kind: EventTextDelta, TextDelta: ev.TextDelta})
		case "thinking_delta":
			// Accumulate AND surface to the UI. The accumulator feeds
			// the persisted ContentBlock{Type:"thinking"} flushed by
			// flushThinking; the event keeps the live spinner-row
			// "(thinking…)" preview working.
			curThinking += ev.TextDelta
			emit(ctx, out, Event{Kind: EventThinkingDelta, TextDelta: ev.TextDelta})
		case "redacted_thinking":
			// Anthropic's safety classifier replaced a chunk of the
			// model's reasoning with opaque cipher text. We can't
			// display the plaintext (we don't have it), but we MUST
			// persist the encrypted payload so the next turn can echo
			// it back — extended-thinking continuity depends on the
			// model decrypting + reusing this on subsequent turns.
			// Flush any in-flight plaintext thinking first to keep
			// chronological ordering [thinking, redacted_thinking, ...].
			flushThinking()
			blocks = append(blocks, llm.ContentBlock{
				Type: "redacted_thinking",
				Data: ev.TextDelta, // base64 cipher text
			})
			emit(ctx, out, Event{Kind: EventRedactedThinking, TextDelta: ev.TextDelta})
		case "tool_use_start":
			// Same chronology argument as text_delta — a tool call
			// means reasoning has resolved into an action; persist
			// the trace before the tool block.
			flushThinking()
			flushText()
			// Reserve the block at start time, rather than appending it at
			// stop time. Parallel providers may stop calls in any order; the
			// assistant message must retain the model's content-block order.
			blockIndex := len(blocks)
			blocks = append(blocks, llm.ContentBlock{
				Type:      "tool_use",
				ToolUseID: ev.ToolUseID,
				ToolName:  ev.ToolName,
			})
			tool := &streamedTool{blockIndex: blockIndex}
			toolsByID[ev.ToolUseID] = tool
			toolOrder = append(toolOrder, tool)
			// Propagate provider-specific blobs (gemini thoughtSignature,
			// future provider hints) onto the ContentBlock so they round-
			// trip when the message is later re-sent in history. Without
			// this, gemini-3.5+ rejects subsequent turns with "Function
			// call is missing a thought_signature".
			if len(ev.ProviderHint) > 0 {
				blocks[blockIndex].ProviderHint = ev.ProviderHint
			}
		case "tool_input_delta":
			tool := lookupTool(ev.ToolUseID)
			if tool != nil {
				tool.json += ev.InputDelta
			}
			// Forward the partial JSON chunk to the UI so the user sees
			// tool args appear as they're generated — kimi-cli's
			// streamingjson behavior, claude-code parity. ToolUseID lets
			// the UI route the delta to the right in-flight row when
			// multiple tools are spawning in parallel.
			if tool != nil {
				block := blocks[tool.blockIndex]
				emit(ctx, out, Event{
					Kind:      EventToolArgsDelta,
					ToolUseID: block.ToolUseID,
					ToolName:  block.ToolName,
					TextDelta: ev.InputDelta,
				})
			}
		case "tool_use_stop":
			// Provider-side authoritative resync: both anthropic.go and
			// openai.go also accumulate the args server-side and ship
			// the full string via tool_use_stop.InputDelta. If our per-
			// delta accumulation lost bytes (e.g. a single SSE chunk
			// dropped on a flaky connection — surfaced as the MiniMax
			// "internal/jobs/jobs.go → l/jobs/jobs.go" path corruption,
			// where `interna` was eaten between deltas), the provider's
			// full string is the source of truth when it is at least as
			// complete as the incremental buffer. Keep the longer local
			// buffer if a compatibility provider emits a truncated stop
			// payload (covered by ToolUseStopShorterKeepsCurJSON).
			tool := lookupTool(ev.ToolUseID)
			if tool != nil {
				if ev.InputDelta != "" && len(ev.InputDelta) >= len(tool.json) {
					tool.json = ev.InputDelta
				}
				flushTool(tool)
				if toolsByID[ev.ToolUseID] == tool {
					delete(toolsByID, ev.ToolUseID)
				}
			}
		case "message_delta":
			if ev.StopReason != "" {
				stopReas = ev.StopReason
			}
			usage.out = ev.OutputTokens
			// Some Anthropic-compatible gateways (MiniMax, ...) only
			// report input_tokens here, not at message_start. Take the
			// value if it shows up and we haven't seen one already, so
			// the bottom-right token counter doesn't get stuck at the
			// output-only number.
			if usage.in == 0 && ev.InputTokens > 0 {
				usage.in = ev.InputTokens
			}
			if ev.CacheCreationInputTokens > 0 {
				usage.cacheCreate = ev.CacheCreationInputTokens
			}
			if ev.CacheReadInputTokens > 0 {
				usage.cacheRead = ev.CacheReadInputTokens
			}
		case "message_stop":
			flushThinking()
			flushText()
			flushTools()
			return blocks, stopReas, &usage, nil
		case "error":
			return nil, "", nil, ev.Err
		}
	}
}

// filterToolUses returns only the tool_use blocks from an assistant
// message. Used to decide whether the next iteration should dispatch
// tools or terminate the turn.
func filterToolUses(blocks []llm.ContentBlock) []llm.ContentBlock {
	var out []llm.ContentBlock
	for _, b := range blocks {
		if b.Type == "tool_use" {
			out = append(out, b)
		}
	}
	return out
}
