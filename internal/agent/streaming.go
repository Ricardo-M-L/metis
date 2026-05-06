package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/Ricardo-M-L/metis/internal/llm"
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
		blocks   []llm.ContentBlock
		curText  string
		curTool  *llm.ContentBlock
		curJSON  string
		stopReas string
		usage    usageTotals
	)
	flushText := func() {
		if curText != "" {
			blocks = append(blocks, llm.ContentBlock{Type: "text", Text: curText})
			curText = ""
		}
	}
	flushTool := func() {
		if curTool == nil {
			return
		}
		if curJSON != "" {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(curJSON), &parsed); err == nil {
				curTool.ToolInput = parsed
			} else {
				curTool.ToolInput = map[string]any{"_raw": curJSON}
			}
		}
		blocks = append(blocks, *curTool)
		curTool = nil
		curJSON = ""
	}
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			flushText()
			flushTool()
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
			curText += ev.TextDelta
			emit(ctx, out, Event{Kind: EventTextDelta, TextDelta: ev.TextDelta})
		case "thinking_delta":
			// Surface model reasoning to the UI without folding it into
			// the assembled assistant blocks — thinking is a transient
			// trace, not part of the persisted message that downstream
			// turns will see.
			emit(ctx, out, Event{Kind: EventThinkingDelta, TextDelta: ev.TextDelta})
		case "tool_use_start":
			flushText()
			curTool = &llm.ContentBlock{Type: "tool_use", ToolUseID: ev.ToolUseID, ToolName: ev.ToolName}
			curJSON = ""
		case "tool_input_delta":
			curJSON += ev.InputDelta
			// Forward the partial JSON chunk to the UI so the user sees
			// tool args appear as they're generated — kimi-cli's
			// streamingjson behavior, claude-code parity. ToolUseID lets
			// the UI route the delta to the right in-flight row when
			// multiple tools are spawning in parallel.
			if curTool != nil {
				emit(ctx, out, Event{
					Kind:      EventToolArgsDelta,
					ToolUseID: curTool.ToolUseID,
					ToolName:  curTool.ToolName,
					TextDelta: ev.InputDelta,
				})
			}
		case "tool_use_stop":
			flushTool()
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
			flushText()
			flushTool()
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
