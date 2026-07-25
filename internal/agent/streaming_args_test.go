package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// mockStream replays a fixed sequence of StreamEvents. Returns io.EOF
// once exhausted so consumeStream's terminal branch fires.
type mockStream struct {
	events []llm.StreamEvent
	idx    int
}

func (m *mockStream) Recv() (llm.StreamEvent, error) {
	if m.idx >= len(m.events) {
		return llm.StreamEvent{}, io.EOF
	}
	e := m.events[m.idx]
	m.idx++
	return e, nil
}

func (m *mockStream) Close() error { return nil }

// TestConsumeStream_EmitsToolArgsDelta — driving consumeStream with
// tool_input_delta wire events should now produce per-chunk
// EventToolArgsDelta on the out channel (T12). Before this change the
// chunks were silently accumulated until tool_use_stop; the UI saw
// the args only after the full tool_use block closed.
func TestConsumeStream_EmitsToolArgsDelta(t *testing.T) {
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "message_start", InputTokens: 100},
		{Type: "tool_use_start", ToolUseID: "t1", ToolName: "Read"},
		{Type: "tool_input_delta", ToolUseID: "t1", InputDelta: `{"path":`},
		{Type: "tool_input_delta", ToolUseID: "t1", InputDelta: `"/tmp/foo`},
		{Type: "tool_input_delta", ToolUseID: "t1", InputDelta: `.go"}`},
		{Type: "tool_use_stop", ToolUseID: "t1"},
		{Type: "message_stop"},
	}}

	out := make(chan Event, 32)
	loop := &Loop{}
	go func() {
		_, _, _, _ = loop.consumeStream(context.Background(), stream, out)
		close(out)
	}()

	var (
		argsDeltas    []string
		argsDeltaTool string
	)
	for ev := range out {
		if ev.Kind == EventToolArgsDelta {
			argsDeltas = append(argsDeltas, ev.TextDelta)
			argsDeltaTool = ev.ToolName
		}
	}

	if len(argsDeltas) != 3 {
		t.Errorf("expected 3 EventToolArgsDelta events; got %d (%v)",
			len(argsDeltas), argsDeltas)
	}
	if joined := strings.Join(argsDeltas, ""); joined != `{"path":"/tmp/foo.go"}` {
		t.Errorf("concatenated deltas should reassemble the full JSON; got %q", joined)
	}
	if argsDeltaTool != "Read" {
		t.Errorf("EventToolArgsDelta should carry ToolName; got %q", argsDeltaTool)
	}
}

// TestConsumeStream_InterleavedParallelTools keeps one independent JSON
// accumulator per tool_use_id. OpenAI-compatible providers can interleave
// argument deltas for several calls in one response; the old single curTool
// slot overwrote earlier calls and reduced every batch to its final tool.
func TestConsumeStream_InterleavedParallelTools(t *testing.T) {
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "message_start"},
		{Type: "tool_use_start", ToolUseID: "read-a", ToolName: "Read"},
		{Type: "tool_input_delta", ToolUseID: "read-a", InputDelta: `{"path":"/tmp/a`},
		{Type: "tool_use_start", ToolUseID: "grep-b", ToolName: "Grep"},
		{Type: "tool_input_delta", ToolUseID: "grep-b", InputDelta: `{"pattern":"TODO`},
		{Type: "tool_input_delta", ToolUseID: "read-a", InputDelta: `.go"}`},
		{Type: "tool_input_delta", ToolUseID: "grep-b", InputDelta: `","root":"/tmp"}`},
		// Stop in reverse order. Content-block order must still follow starts.
		{Type: "tool_use_stop", ToolUseID: "grep-b"},
		{Type: "tool_use_stop", ToolUseID: "read-a"},
		{Type: "message_stop", StopReason: "tool_use"},
	}}
	out := make(chan Event, 32)
	loop := &Loop{}
	var blocks []llm.ContentBlock
	done := make(chan struct{})
	go func() {
		blocks, _, _, _ = loop.consumeStream(context.Background(), stream, out)
		close(out)
		close(done)
	}()
	var deltaIDs []string
	for ev := range out {
		if ev.Kind == EventToolArgsDelta {
			deltaIDs = append(deltaIDs, ev.ToolUseID)
		}
	}
	<-done

	if len(blocks) != 2 {
		t.Fatalf("expected both parallel tool blocks, got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].ToolUseID != "read-a" || blocks[0].ToolName != "Read" {
		t.Errorf("block[0] lost start order: %+v", blocks[0])
	}
	if blocks[1].ToolUseID != "grep-b" || blocks[1].ToolName != "Grep" {
		t.Errorf("block[1] lost start order: %+v", blocks[1])
	}
	if got, _ := blocks[0].ToolInput["path"].(string); got != "/tmp/a.go" {
		t.Errorf("read args crossed streams: got %q (%+v)", got, blocks[0].ToolInput)
	}
	if got, _ := blocks[1].ToolInput["pattern"].(string); got != "TODO" {
		t.Errorf("grep args crossed streams: got %q (%+v)", got, blocks[1].ToolInput)
	}
	if got, _ := blocks[1].ToolInput["root"].(string); got != "/tmp" {
		t.Errorf("grep root missing: got %q (%+v)", got, blocks[1].ToolInput)
	}
	if strings.Join(deltaIDs, ",") != "read-a,grep-b,read-a,grep-b" {
		t.Errorf("args delta attribution = %v", deltaIDs)
	}
}

// TestConsumeStream_NoArgsDeltaWithoutToolStart — stray
// tool_input_delta before any tool_use_start should NOT emit an args
// event (no in-flight tool to attribute it to). Defensive — guards
// against malformed provider streams crashing the loop.
func TestConsumeStream_NoArgsDeltaWithoutToolStart(t *testing.T) {
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "message_start", InputTokens: 1},
		{Type: "tool_input_delta", ToolUseID: "orphan", InputDelta: `{"x":1}`},
		{Type: "message_stop"},
	}}

	out := make(chan Event, 8)
	loop := &Loop{}
	go func() {
		_, _, _, _ = loop.consumeStream(context.Background(), stream, out)
		close(out)
	}()

	for ev := range out {
		if ev.Kind == EventToolArgsDelta {
			t.Errorf("orphan tool_input_delta must NOT emit EventToolArgsDelta; got %+v", ev)
		}
	}
}

// TestConsumeStream_PersistsThinkingBlock — thinking_delta accumulates
// into a ContentBlock{Type:"thinking"} on the assistant message so it
// survives the session jsonl round-trip (and shows up after /resume).
// Earlier metis treated thinking as transient TUI-only — flushed
// blocks had no thinking, the user lost the reasoning trace on resume.
//
// The flushed blocks must be ordered [thinking, text] for the chunk
// where thinking precedes text — Anthropic's wire format expects this
// chronology and the TUI render reads sequentially.
func TestConsumeStream_PersistsThinkingBlock(t *testing.T) {
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "message_start"},
		{Type: "thinking_delta", TextDelta: "let me check "},
		{Type: "thinking_delta", TextDelta: "the file structure"},
		{Type: "text_delta", TextDelta: "Here's the answer."},
		{Type: "message_stop"},
	}}
	out := make(chan Event, 32)
	loop := &Loop{}
	var blocks []llm.ContentBlock
	done := make(chan struct{})
	go func() {
		blocks, _, _, _ = loop.consumeStream(context.Background(), stream, out)
		close(out)
		close(done)
	}()
	// Drain so the goroutine doesn't block.
	for range out {
	}
	<-done

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (thinking + text); got %d (%+v)", len(blocks), blocks)
	}
	if blocks[0].Type != "thinking" {
		t.Errorf("blocks[0].Type = %q, want \"thinking\"", blocks[0].Type)
	}
	if blocks[0].Text != "let me check the file structure" {
		t.Errorf("blocks[0].Text = %q, want concatenated thinking", blocks[0].Text)
	}
	if blocks[1].Type != "text" || blocks[1].Text != "Here's the answer." {
		t.Errorf("blocks[1] = %+v, want {text, \"Here's the answer.\"}", blocks[1])
	}
}

// TestConsumeStream_ThinkingFlushedBeforeTool — thinking that resolves
// into a tool call must persist BEFORE the tool_use block in the
// assembled message. Mirrors Anthropic's chronology and lets the user
// re-read the reasoning that produced each tool call after /resume.
func TestConsumeStream_ThinkingFlushedBeforeTool(t *testing.T) {
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "message_start"},
		{Type: "thinking_delta", TextDelta: "I should grep for"},
		{Type: "thinking_delta", TextDelta: " TODO comments"},
		{Type: "tool_use_start", ToolUseID: "t1", ToolName: "Grep"},
		{Type: "tool_input_delta", ToolUseID: "t1", InputDelta: `{"pattern":"TODO"}`},
		{Type: "tool_use_stop", ToolUseID: "t1"},
		{Type: "message_stop"},
	}}
	out := make(chan Event, 32)
	loop := &Loop{}
	var blocks []llm.ContentBlock
	done := make(chan struct{})
	go func() {
		blocks, _, _, _ = loop.consumeStream(context.Background(), stream, out)
		close(out)
		close(done)
	}()
	for range out {
	}
	<-done

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (thinking + tool_use); got %d (%+v)", len(blocks), blocks)
	}
	if blocks[0].Type != "thinking" || blocks[0].Text != "I should grep for TODO comments" {
		t.Errorf("blocks[0] = %+v, want {thinking, ...}", blocks[0])
	}
	if blocks[1].Type != "tool_use" || blocks[1].ToolName != "Grep" {
		t.Errorf("blocks[1] = %+v, want {tool_use, Grep}", blocks[1])
	}
}

// TestConsumeStream_ThinkingDeltaStillEmitted — accumulating into the
// persisted block must not break the live-stream EventThinkingDelta
// surface. The TUI's spinner row depends on per-delta events.
func TestConsumeStream_ThinkingDeltaStillEmitted(t *testing.T) {
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "message_start"},
		{Type: "thinking_delta", TextDelta: "hmm"},
		{Type: "thinking_delta", TextDelta: " ok"},
		{Type: "text_delta", TextDelta: "answer"},
		{Type: "message_stop"},
	}}
	out := make(chan Event, 32)
	loop := &Loop{}
	go func() {
		_, _, _, _ = loop.consumeStream(context.Background(), stream, out)
		close(out)
	}()
	var thinkingDeltas []string
	for ev := range out {
		if ev.Kind == EventThinkingDelta {
			thinkingDeltas = append(thinkingDeltas, ev.TextDelta)
		}
	}
	if len(thinkingDeltas) != 2 {
		t.Errorf("expected 2 EventThinkingDelta; got %d (%v)", len(thinkingDeltas), thinkingDeltas)
	}
}

// TestConsumeStream_ToolUseStopResyncsLostBytes — defends against the
// MiniMax char-loss bug surfaced 2026-05-10: a single tool_input_delta
// chunk dropped between provider and agent (the user's actual report
// was "internal/jobs/jobs.go" arriving at the tool as "l/jobs/jobs.go"
// — the prefix `interna` lost between two deltas).
//
// The provider also ships the FULL accumulated args string via
// tool_use_stop.InputDelta as authoritative resync. Without the fix
// the agent trusts only its incrementally-built curJSON; with the fix
// it prefers ev.InputDelta when at least as long, recovering the
// dropped bytes silently.
func TestConsumeStream_ToolUseStopResyncsLostBytes(t *testing.T) {
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "message_start"},
		{Type: "tool_use_start", ToolUseID: "t1", ToolName: "Read"},
		{Type: "tool_input_delta", ToolUseID: "t1", InputDelta: `{"path":"`},
		// Simulate the LOST middle delta: provider's authoritative
		// stop-event still has "interna", but the per-delta path is
		// missing it.
		{Type: "tool_input_delta", ToolUseID: "t1", InputDelta: `l/jobs/jobs.go"}`},
		{Type: "tool_use_stop", ToolUseID: "t1", InputDelta: `{"path":"internal/jobs/jobs.go"}`},
		{Type: "message_stop"},
	}}
	out := make(chan Event, 32)
	loop := &Loop{}
	var blocks []llm.ContentBlock
	done := make(chan struct{})
	go func() {
		blocks, _, _, _ = loop.consumeStream(context.Background(), stream, out)
		close(out)
		close(done)
	}()
	for range out {
	}
	<-done

	// Find the tool_use block — it's the last one (after any thinking/text).
	var toolBlock *llm.ContentBlock
	for i := range blocks {
		if blocks[i].Type == "tool_use" {
			toolBlock = &blocks[i]
		}
	}
	if toolBlock == nil {
		t.Fatalf("no tool_use block produced; blocks=%+v", blocks)
	}
	path, ok := toolBlock.ToolInput["path"].(string)
	if !ok {
		t.Fatalf("ToolInput.path not a string: %+v", toolBlock.ToolInput)
	}
	if path != "internal/jobs/jobs.go" {
		t.Errorf("path = %q, want %q (resync should recover from lost middle delta)",
			path, "internal/jobs/jobs.go")
	}
}

// TestConsumeStream_ToolUseStopShorterKeepsCurJSON — defensive: if
// the provider sends a shorter InputDelta on stop than we accumulated,
// we DON'T trim. That'd be the inverse bug (provider lost bytes, we
// have them all). curJSON wins.
func TestConsumeStream_ToolUseStopShorterKeepsCurJSON(t *testing.T) {
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "message_start"},
		{Type: "tool_use_start", ToolUseID: "t1", ToolName: "Read"},
		{Type: "tool_input_delta", ToolUseID: "t1", InputDelta: `{"path":"internal/jobs/jobs.go"}`},
		// Provider's stop-event truncated (hypothetical); agent
		// should keep the longer per-delta accumulation.
		{Type: "tool_use_stop", ToolUseID: "t1", InputDelta: `{"path":"l"}`},
		{Type: "message_stop"},
	}}
	out := make(chan Event, 32)
	loop := &Loop{}
	var blocks []llm.ContentBlock
	done := make(chan struct{})
	go func() {
		blocks, _, _, _ = loop.consumeStream(context.Background(), stream, out)
		close(out)
		close(done)
	}()
	for range out {
	}
	<-done

	var toolBlock *llm.ContentBlock
	for i := range blocks {
		if blocks[i].Type == "tool_use" {
			toolBlock = &blocks[i]
		}
	}
	if toolBlock == nil {
		t.Fatalf("no tool_use block; blocks=%+v", blocks)
	}
	if path, _ := toolBlock.ToolInput["path"].(string); path != "internal/jobs/jobs.go" {
		t.Errorf("path = %q, want %q (longer curJSON wins)", path, "internal/jobs/jobs.go")
	}
}

// TestConsumeStream_UnwrapsMinimaxBundledArgs — 2026-05-26 regression
// for session 87e366fa. MiniMax-M2.7 anthropic-shim emits cu mouse_move
// tool calls as `{"_": "{\"x\":735,\"y\":130}"}` instead of the spec
// `{"x":735,"y":130}` shape, and every cu call subsequently failed
// with `missing required field: x`. The stream consumer must now
// unwrap that bundle before the tool dispatcher sees it.
func TestConsumeStream_UnwrapsMinimaxBundledArgs(t *testing.T) {
	bundled := `{"_": "{\"x\":735,\"y\":130}"}`
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "message_start"},
		{Type: "tool_use_start", ToolUseID: "t1", ToolName: "mcp__computer-use__mouse_move"},
		{Type: "tool_input_delta", ToolUseID: "t1", InputDelta: bundled},
		{Type: "tool_use_stop", ToolUseID: "t1", InputDelta: bundled},
		{Type: "message_stop"},
	}}
	out := make(chan Event, 32)
	loop := &Loop{}
	var blocks []llm.ContentBlock
	done := make(chan struct{})
	go func() {
		blocks, _, _, _ = loop.consumeStream(context.Background(), stream, out)
		close(out)
		close(done)
	}()
	for range out {
	}
	<-done

	var toolBlock *llm.ContentBlock
	for i := range blocks {
		if blocks[i].Type == "tool_use" {
			toolBlock = &blocks[i]
		}
	}
	if toolBlock == nil {
		t.Fatalf("no tool_use block; blocks=%+v", blocks)
	}
	if _, isWrapper := toolBlock.ToolInput["_"]; isWrapper {
		t.Fatalf("MiniMax `_` wrapper survived to dispatcher; got %+v", toolBlock.ToolInput)
	}
	if x, _ := toolBlock.ToolInput["x"].(float64); x != 735 {
		t.Errorf("x = %v, want 735 (unwrap should expose the embedded field)", toolBlock.ToolInput["x"])
	}
	if y, _ := toolBlock.ToolInput["y"].(float64); y != 130 {
		t.Errorf("y = %v, want 130 (unwrap should expose the embedded field)", toolBlock.ToolInput["y"])
	}
}

// TestConsumeStream_DoesNotUnwrapNormalArgs — defensive twin of the
// regression above: a well-formed cu screenshot call must NOT be
// touched by the unwrap path. Pins the "保证别影响别的模型" guarantee
// at the stream level so a future refactor of argsunwrap's guard can't
// silently widen the trigger.
func TestConsumeStream_DoesNotUnwrapNormalArgs(t *testing.T) {
	stream := &mockStream{events: []llm.StreamEvent{
		{Type: "message_start"},
		{Type: "tool_use_start", ToolUseID: "t1", ToolName: "Read"},
		{Type: "tool_input_delta", ToolUseID: "t1", InputDelta: `{"path":"/tmp/foo.go"}`},
		{Type: "tool_use_stop", ToolUseID: "t1", InputDelta: `{"path":"/tmp/foo.go"}`},
		{Type: "message_stop"},
	}}
	out := make(chan Event, 32)
	loop := &Loop{}
	var blocks []llm.ContentBlock
	done := make(chan struct{})
	go func() {
		blocks, _, _, _ = loop.consumeStream(context.Background(), stream, out)
		close(out)
		close(done)
	}()
	for range out {
	}
	<-done

	var toolBlock *llm.ContentBlock
	for i := range blocks {
		if blocks[i].Type == "tool_use" {
			toolBlock = &blocks[i]
		}
	}
	if toolBlock == nil {
		t.Fatalf("no tool_use block; blocks=%+v", blocks)
	}
	if path, _ := toolBlock.ToolInput["path"].(string); path != "/tmp/foo.go" {
		t.Errorf("normal args were tampered with; got %+v", toolBlock.ToolInput)
	}
}
