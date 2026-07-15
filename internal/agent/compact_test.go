package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// fakeSummarizer returns "MOCK_SUMMARY" for any call. Implements the
// streaming path now that compact.go switched off Complete — the Stream
// shim feeds two events (a text_delta and a message_stop) so the
// streaming consumer in compact.go assembles the same string the old
// Complete path used to return.
type fakeSummarizer struct {
	calls   int
	lastReq llm.Request
}

func (f *fakeSummarizer) Name() string          { return "fake" }
func (f *fakeSummarizer) MaxContextTokens() int { return 100000 }
func (f *fakeSummarizer) ModelID() string       { return "" }
func (f *fakeSummarizer) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	f.calls++
	f.lastReq = req
	return &fakeStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "MOCK_SUMMARY"},
		{Type: "message_stop"},
	}}, nil
}
func (f *fakeSummarizer) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return nil, errors.New("compact_test: Complete should not be called — compaction streams now")
}

// fakeStream is a tiny llm.StreamReader that walks a fixed slice of
// events. Returns io.EOF after the last event so consumeStream knows
// the stream is done.
type fakeStream struct {
	events []llm.StreamEvent
	idx    int
}

func (s *fakeStream) Recv() (llm.StreamEvent, error) {
	if s.idx >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}
func (s *fakeStream) Close() error { return nil }

// --- helpers ---------------------------------------------------------------

func msg(role llm.Role, text string) llm.Message {
	return llm.Message{Role: role, Content: []llm.ContentBlock{{Type: "text", Text: text}}}
}

func toolUseMsg(id, name string) llm.Message {
	return llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			{Type: "text", Text: "calling tool"},
			{Type: "tool_use", ToolUseID: id, ToolName: name, ToolInput: map[string]any{}},
		},
	}
}

func toolResultMsg(id, output string) llm.Message {
	return llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: id, ToolResult: output},
		},
	}
}

func newCompactorFor(p llm.Provider) *Compactor {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	return NewCompactor(cfg, "test-model", 1000, p)
}

// --- tests -----------------------------------------------------------------

func TestCompact_NoOpWhenSmall(t *testing.T) {
	p := &fakeSummarizer{}
	c := newCompactorFor(p)

	msgs := []llm.Message{
		msg(llm.RoleSystem, "system"),
		msg(llm.RoleUser, "u1"),
		msg(llm.RoleAssistant, "a1"),
	}
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(msgs) {
		t.Errorf("expected no-op, got %d messages (was %d)", len(out), len(msgs))
	}
	if p.calls != 0 {
		t.Errorf("summarizer should not be called for short conversations")
	}
}

func TestCompact_ProducesBoundaryWithSummary(t *testing.T) {
	p := &fakeSummarizer{}
	c := newCompactorFor(p)

	// 1 system + 6 mid + 3 tail = 10 messages, well over the threshold
	msgs := []llm.Message{msg(llm.RoleSystem, "sys")}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, msg(llm.RoleUser, "old user "+istr(i)))
		msgs = append(msgs, msg(llm.RoleAssistant, "old asst "+istr(i)))
	}
	for i := 0; i < 3; i++ {
		msgs = append(msgs, msg(llm.RoleAssistant, "recent "+istr(i)))
	}

	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Errorf("expected 1 summarize call, got %d", p.calls)
	}
	// Layout: 1 sys + 12 mid (alternating user/asst, last is asst @ msgs[12])
	// + 3 recent asst (msgs[13..15]) = 16 total.
	//
	// keepLast (msgs[13:16]) is purely assistant → 2026-05-13 anchor
	// fix detects "no user-text in keepLast" and pulls cut back to
	// the last user-text msg (msgs[11] = "old user 5"). Post-compact
	// keepLast becomes msgs[11:16] = 5 messages (1 user + 4 asst).
	//
	// Expect: 1 keepFirst + 1 boundary + 5 keepLast = 7.
	//
	// (Old behavior was 6 — keepFirst + boundary + synthetic-user-ack +
	// 3 keepLast — because the pre-2026-05-13 slicer didn't preserve
	// the active user prompt and the boundary asst → keepLast asst
	// run required a synthetic ack to maintain user/asst alternation.
	// New behavior is better: real user prompt survives verbatim,
	// alternation is natural (boundary asst → keepLast user), no
	// synthetic ack needed.)
	//
	// Boundary is RoleAssistant (not RoleSystem) — bug #10 2026-04-30:
	// MiniMax + strict Anthropic reject mid-array system role with
	// error 2013, so the boundary is rendered narratively as if the
	// assistant said "I summarized our earlier conversation."
	if len(out) != 7 {
		t.Fatalf("expected 7 messages after compact (anchor preserves user-text), got %d", len(out))
	}
	if out[1].Role != llm.RoleAssistant {
		t.Errorf("boundary should be assistant role (mid-array system rejected by APIs), got %q", out[1].Role)
	}
	if len(out[1].Content) == 0 || !strings.Contains(out[1].Content[0].Text, "MOCK_SUMMARY") {
		t.Errorf("boundary missing summary, got: %v", out[1].Content)
	}
	// out[2] is keepLast[0] = msgs[11] = "old user 5" (USER role).
	// Alternation is natural — no synthetic ack required.
	if out[2].Role != llm.RoleUser {
		t.Errorf("keepLast[0] (anchor-preserved user-text) expected at out[2], got role=%q", out[2].Role)
	}
	if len(out[2].Content) == 0 || !strings.Contains(out[2].Content[0].Text, "old user 5") {
		t.Errorf("out[2] should be the active-task anchor msgs[11]; got %v", out[2].Content)
	}
}

// TestCompact_BoundaryIsNeverSystem locks in bug #10 fix: the boundary
// message MUST NOT use RoleSystem because MiniMax (and strict
// Anthropic) reject mid-array system role with error 2013. RoleAssistant
// is the only safe choice that doesn't break user/assistant alternation
// for the typical user-first keepLast.
func TestCompact_BoundaryIsNeverSystem(t *testing.T) {
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	msgs := []llm.Message{msg(llm.RoleSystem, "sys")}
	for i := 0; i < 5; i++ {
		msgs = append(msgs, msg(llm.RoleUser, "u"+istr(i)))
		msgs = append(msgs, msg(llm.RoleAssistant, "a"+istr(i)))
	}
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range out {
		// keepFirst[0] is allowed to be system (the original prompt
		// passes through), but no SUBSEQUENT message can be.
		if i == 0 {
			continue
		}
		if m.Role == llm.RoleSystem {
			t.Errorf("compacted output[%d] has RoleSystem (would trigger MiniMax error 2013): %+v", i, m)
		}
	}
}

func TestCompact_ToolPairProtectsResult(t *testing.T) {
	// Build: [sys, u1, a1, asst[tool_use X], user[tool_result X], a-recent, a-recent, a-recent]
	// ProtectFirst=1, ProtectLast=3 → cut = 8-3 = 5, which lands ON the user[tool_result X].
	// Without protection we'd keep [a-recent x3] starting with an orphan tool_result is impossible
	// because the orphan would be inside keepLast — we actually want the assistant tool_use
	// to be the one preserved.
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	msgs := []llm.Message{
		msg(llm.RoleSystem, "sys"),
		msg(llm.RoleUser, "u1"),
		msg(llm.RoleAssistant, "a1"),
		toolUseMsg("call-1", "Read"),
		toolResultMsg("call-1", "file contents"),
		msg(llm.RoleAssistant, "recent1"),
		msg(llm.RoleAssistant, "recent2"),
		msg(llm.RoleAssistant, "recent3"),
	}
	// raw cut = 8 - 3 = 5 → messages[5] is "recent1", no tool_result, no adjustment.
	// But let's use a case where cut DOES land on a tool_result:
	c.ProtectLast = 4
	// Now cut = 8 - 4 = 4 → messages[4] is the tool_result, must back up to 3.
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	// After adjustment: keepFirst=1 + boundary=1(asst) + ack=1(user, since
	// keepLast[0] is the tool_use which is asst-role) + keepLast=5 = 8.
	if len(out) != 1+1+1+5 {
		t.Fatalf("expected 8 messages after pair-aware compact, got %d", len(out))
	}
	// out[3] starts the kept tail (after keepFirst + boundary + ack);
	// must be the tool_use, not an orphan tool_result.
	tail := out[3]
	hasToolUse := false
	for _, b := range tail.Content {
		if b.Type == "tool_use" {
			hasToolUse = true
			break
		}
	}
	if !hasToolUse {
		t.Errorf("kept tail should start with the tool_use (matching the preserved tool_result), got: %+v", tail)
	}
}

func TestCompact_NoOpWhenAdjustmentSwallowsMiddle(t *testing.T) {
	// Construct a conversation where every message after the first is a tool_result
	// (pathological case). adjustCutForToolPairs will walk all the way back to
	// ProtectFirst and Compact should bail out without summarizing.
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	msgs := []llm.Message{msg(llm.RoleSystem, "sys")}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, toolResultMsg("call-"+istr(i), "ignored"))
	}
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(msgs) {
		t.Errorf("expected no-op when adjustment swallows middle, got len=%d (orig=%d)", len(out), len(msgs))
	}
	if p.calls != 0 {
		t.Errorf("summarizer should not be called when no compaction is possible")
	}
}

func TestCompact_KeepLastSliceCorrect(t *testing.T) {
	// Regression: original code used messages[ProtectLast:] which is wrong when
	// len != 2*ProtectLast. Build a 12-message conversation; verify ProtectLast=5
	// keeps last 5 messages (natural slicing, since keepLast already has user-text
	// — every msg is user-text — the 2026-05-13 anchor fix is a no-op here).
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 5
	c := NewCompactor(cfg, "m", 1000, p)

	msgs := []llm.Message{msg(llm.RoleSystem, "sys")}
	for i := 0; i < 11; i++ {
		msgs = append(msgs, msg(llm.RoleUser, "msg "+istr(i)))
	}
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	// Layout: 1 sys + 11 user. cut = 12-5 = 7. keepLast = msgs[7:12] = 5
	// user-text msgs ("msg 6" through "msg 10"). Anchor fix detects
	// keepLast already has user-text and skips intervention.
	//
	// keepFirst (1) + boundary asst (1) + synthetic user ack (1 —
	// keepLast[0] is RoleUser, so no ack needed actually).
	//
	// Wait: boundary is asst, keepLast[0]=user → alternation natural,
	// no synthetic ack. Total = 1 + 1 + 5 = 7.
	if len(out) != 7 {
		t.Fatalf("expected 7 messages, got %d", len(out))
	}
	// Last kept message must be the original last one ("msg 10").
	last := out[len(out)-1]
	if last.Content[0].Text != "msg 10" {
		t.Errorf("last message wrong: %q", last.Content[0].Text)
	}
}

func TestAdjustCutForToolPairs_DoesNotMoveOnNormalMessage(t *testing.T) {
	msgs := []llm.Message{
		msg(llm.RoleUser, "u"),
		msg(llm.RoleAssistant, "a"),
		msg(llm.RoleUser, "u2"),
	}
	if got := adjustCutForToolPairs(msgs, 2); got != 2 {
		t.Errorf("cut moved unexpectedly: %d", got)
	}
}

func TestAdjustCutForToolPairs_BacksUpOverToolResult(t *testing.T) {
	msgs := []llm.Message{
		msg(llm.RoleUser, "u"),
		toolUseMsg("x", "Read"),
		toolResultMsg("x", "data"),
	}
	if got := adjustCutForToolPairs(msgs, 2); got != 1 {
		t.Errorf("cut should back up to 1, got %d", got)
	}
}

func TestEstimateTokens(t *testing.T) {
	msgs := []llm.Message{
		msg(llm.RoleUser, strings.Repeat("a", 400)),
		msg(llm.RoleAssistant, strings.Repeat("b", 400)),
	}
	got := estimateTokens(msgs)
	// Per message: 4 (role) + per block: 8 (envelope) + 100 (text/4) = 112.
	// Two messages → 224.
	if got != 224 {
		t.Errorf("estimateTokens = %d, want 224", got)
	}
}

// TestEstimateTokens_ToolHeavy locks in that tool inputs and tool_results
// are no longer invisible to the estimator — the previous body-only
// estimator missed the bulk of token consumption in tool-heavy turns,
// which let auto-compaction silently undercount and the request
// overflow the provider's context window mid-stream.
func TestEstimateTokens_ToolHeavy(t *testing.T) {
	msgs := []llm.Message{
		{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{{
				Type:      "tool_use",
				ToolName:  "bash",
				ToolUseID: "tu_1",
				ToolInput: map[string]any{"command": strings.Repeat("x", 400)},
			}},
		},
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Type:       "tool_result",
				ToolUseID:  "tu_1",
				ToolResult: strings.Repeat("y", 800),
			}},
		},
	}
	got := estimateTokens(msgs)
	// Sanity: must be > body-only zero (Text empty for both) and reflect
	// at least the 400 tool-input + 800 tool-result bytes (~300 tokens).
	if got < 250 {
		t.Errorf("estimateTokens should count tool inputs + results; got %d", got)
	}
}

func TestShouldCompact(t *testing.T) {
	p := &fakeSummarizer{}
	// maxCtx=200, threshold=0.5 → triggers at >= 100 tokens
	c := NewCompactor(Config{Threshold: 0.5}, "m", 200, p)
	short := []llm.Message{msg(llm.RoleUser, "hi")} // ~50 tokens
	if c.ShouldCompact(short) {
		t.Error("should not compact short conversation")
	}
	long := []llm.Message{msg(llm.RoleUser, strings.Repeat("x", 800))} // 50 + 200 = 250 tokens
	if !c.ShouldCompact(long) {
		t.Error("should compact long conversation past threshold")
	}
}

// TestShouldCompact_AccountsForMaxTokens locks in the bug-#6 fix: the
// effective input cap subtracts the per-request max_tokens budget so
// compaction fires *before* the API's `input + max_tokens > window`
// check rejects the next request.
//
// Repro of the user's MiniMax-M2.7 case in miniature:
//   - context window 1000, max_tokens 400, threshold 0.85
//   - effective input cap = 1000 - 400 = 600
//   - threshold trigger    = 600 * 0.85 = 510 tokens
//
// Without the fix the threshold would be 1000 * 0.85 = 850, so a 700-
// token conversation would NOT compact, then the next request (700
// input + 400 max_tokens = 1100 > 1000) would be rejected by the API.
func TestShouldCompact_AccountsForMaxTokens(t *testing.T) {
	p := &fakeSummarizer{}
	c := NewCompactor(Config{Threshold: 0.85}, "m", 1000, p)
	c.MaxOutputTokens = 400

	// 700 tokens (under raw 850 threshold, OVER effective 510 threshold).
	// estimateTokens charges ~12 tokens of envelope per message + 4 chars/token
	// for text, so ~2700 chars of body tips us over 700 tokens of input.
	mid := []llm.Message{msg(llm.RoleUser, strings.Repeat("x", 2700))}
	if !c.ShouldCompact(mid) {
		t.Errorf("should compact: input ~700 tokens > effective threshold (1000-400)*0.85=510")
	}

	// Verify the legacy path: when MaxOutputTokens is 0, falls back to
	// using full MaxContextTokens (so the original 850 threshold applies
	// and the same 700-token convo does NOT compact).
	c.MaxOutputTokens = 0
	if c.ShouldCompact(mid) {
		t.Errorf("legacy path: 700 tokens < 1000*0.85=850 threshold should NOT compact")
	}
}

// TestEffectiveInputCap_DefaultsSafely covers the degenerate config:
// max_tokens accidentally set bigger than context_window. We don't want
// effectiveInputCap to go negative or zero (would compact every turn).
func TestEffectiveInputCap_DefaultsSafely(t *testing.T) {
	c := &Compactor{
		MaxContextTokens: 1000,
		MaxOutputTokens:  2000, // user mis-config
	}
	if got := c.effectiveInputCap(); got != 1000 {
		t.Errorf("oversized MaxOutputTokens should fall back to full window; got %d", got)
	}
}

func istr(n int) string {
	return itoa(n)
}

// errSummarizer fails every Stream call. Lets us drive the circuit
// breaker without hitting a real provider.
type errSummarizer struct{ err error }

func (e *errSummarizer) Name() string          { return "err-fake" }
func (e *errSummarizer) MaxContextTokens() int { return 100000 }
func (e *errSummarizer) ModelID() string       { return "" }
func (e *errSummarizer) Stream(_ context.Context, _ llm.Request) (llm.StreamReader, error) {
	return nil, e.err
}
func (e *errSummarizer) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return nil, e.err
}

// TestCompactor_CircuitBreaker_TripsAfter3Failures — the breaker exists
// to short-circuit the runaway-compaction pattern claude-code reported
// (1279 sessions stuck in 50+ failed-compact loops, max 3272 attempts).
// After 3 consecutive failed Compact() calls, ShouldCompact must return
// false even when the input is well over the threshold.
func TestCompactor_CircuitBreaker_TripsAfter3Failures(t *testing.T) {
	p := &errSummarizer{err: errors.New("simulated upstream failure")}
	c := newCompactorFor(p)

	// Build a conversation big enough that ShouldCompact would normally
	// trigger every iteration.
	msgs := []llm.Message{msg(llm.RoleUser, "system seed")}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, msg(llm.RoleUser, strings.Repeat("x", 600)))
		msgs = append(msgs, msg(llm.RoleAssistant, strings.Repeat("y", 600)))
	}
	if !c.ShouldCompact(msgs) {
		t.Fatalf("precondition: convo should trigger compaction before circuit trips")
	}

	for i := 1; i <= MaxConsecutiveCompactFailures; i++ {
		_, err := c.Compact(context.Background(), msgs)
		if err == nil {
			t.Fatalf("attempt %d: expected error from failing summarizer", i)
		}
	}
	if !c.CircuitTripped() {
		t.Errorf("after %d failures CircuitTripped()=true expected", MaxConsecutiveCompactFailures)
	}
	if c.ShouldCompact(msgs) {
		t.Errorf("ShouldCompact must return false once breaker is open, even on oversized input")
	}
}

// TestCompactor_CircuitBreaker_SuccessResetsCounter — a single successful
// compaction must clear the failure counter so transient errors don't
// permanently lock out compaction. Mirrors claude-code's reset-on-success
// semantics.
func TestCompactor_CircuitBreaker_SuccessResetsCounter(t *testing.T) {
	good := &fakeSummarizer{}
	c := newCompactorFor(good)

	// Pre-arm the counter to one-below-trip so the next failure would
	// trip it. We mutate the field directly (test-only access via same
	// package) rather than driving 2 fake failures.
	c.consecutiveFailures = MaxConsecutiveCompactFailures - 1

	msgs := []llm.Message{msg(llm.RoleUser, "seed")}
	for i := 0; i < 8; i++ {
		msgs = append(msgs, msg(llm.RoleUser, strings.Repeat("x", 600)))
		msgs = append(msgs, msg(llm.RoleAssistant, strings.Repeat("y", 600)))
	}
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatalf("good summarizer returned error: %v", err)
	}
	if len(out) >= len(msgs) {
		t.Fatalf("compaction did not progress; got %d, want < %d", len(out), len(msgs))
	}
	if c.consecutiveFailures != 0 {
		t.Errorf("success path must zero consecutiveFailures; got %d", c.consecutiveFailures)
	}
	if c.CircuitTripped() {
		t.Errorf("circuit must NOT be tripped after a successful compaction")
	}
}

// TestCompactor_CircuitBreaker_ResetCircuitReopens — /clear and Loop.Reset
// both must re-enable compaction. Without this the user has no recovery
// path once the breaker trips.
func TestCompactor_CircuitBreaker_ResetCircuitReopens(t *testing.T) {
	c := &Compactor{}
	c.consecutiveFailures = MaxConsecutiveCompactFailures
	if !c.CircuitTripped() {
		t.Fatalf("precondition: setup should leave breaker tripped")
	}
	c.ResetCircuit()
	if c.CircuitTripped() {
		t.Errorf("ResetCircuit() did not clear the breaker state")
	}
}

// --- Snip (Task #67) ----------------------------------------------------

// TestSnip_TruncatesOversizedToolResult — the core contract: tool_result
// blocks longer than SnipMaxToolResultChars get the head + a truthful
// "[snipped: N chars omitted]" tail. Smaller blocks pass through
// untouched.
func TestSnip_TruncatesOversizedToolResult(t *testing.T) {
	c := newCompactorFor(&fakeSummarizer{})
	// Build a long tool_result and a short one. ProtectFirst=1,
	// ProtectLast=3; we need both blocks to land in the snippable middle.
	long := strings.Repeat("a", 5000)
	short := "short result"
	msgs := []llm.Message{
		msg(llm.RoleUser, "system seed"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", long),
		toolUseMsg("t2", "Read"),
		toolResultMsg("t2", short),
		msg(llm.RoleUser, "tail-1"),
		msg(llm.RoleAssistant, "tail-2"),
		msg(llm.RoleUser, "tail-3"),
	}
	out := c.Snip(msgs)

	if len(out) != len(msgs) {
		t.Fatalf("Snip must not change message count; got %d, want %d", len(out), len(msgs))
	}
	// The long tool_result should be truncated.
	gotLong := out[2].Content[0].ToolResult
	if len(gotLong) >= 5000 {
		t.Errorf("long tool_result was not truncated; len=%d", len(gotLong))
	}
	if !strings.HasPrefix(gotLong, strings.Repeat("a", c.SnipMaxToolResultChars)) {
		t.Errorf("truncated result must keep the head; got prefix %q", gotLong[:50])
	}
	if !strings.Contains(gotLong, "[snipped:") {
		t.Errorf("truncated result must carry the snip marker; got %q", gotLong[len(gotLong)-100:])
	}
	// The short tool_result should be untouched.
	if out[4].Content[0].ToolResult != short {
		t.Errorf("short result was modified: %q", out[4].Content[0].ToolResult)
	}
}

// TestSnip_DoesNotTouchProtectedTail — the recent ProtectLast messages
// must stay byte-identical so the agent's most-recent reasoning isn't
// disrupted mid-thought.
func TestSnip_DoesNotTouchProtectedTail(t *testing.T) {
	c := newCompactorFor(&fakeSummarizer{})
	long := strings.Repeat("z", 5000)
	msgs := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", long), // in middle, snippable
		// ProtectLast = 3 → last 3 are kept intact:
		toolUseMsg("t2", "Bash"),
		toolResultMsg("t2", long), // protected
		msg(llm.RoleAssistant, "final answer"),
	}
	out := c.Snip(msgs)

	// out[2] is in the snippable region — should be truncated.
	if len(out[2].Content[0].ToolResult) >= 5000 {
		t.Errorf("middle tool_result should be snipped; len=%d", len(out[2].Content[0].ToolResult))
	}
	// out[4] is in the protected tail — must be byte-equal to original.
	if out[4].Content[0].ToolResult != long {
		t.Errorf("protected-tail tool_result was mutated; got len=%d, want %d", len(out[4].Content[0].ToolResult), len(long))
	}
}

// TestEnforcePostCompactBudget_CapsAllToolResults — every retained
// tool_result, including the protected tail and including any
// already-snipped-with-the-turn-time-cap blocks, gets clamped at
// PostCompactMaxToolResultChars (5000). This is what stops a single
// 50MB grep tail from re-overflowing the very next request after
// compaction completes.
func TestEnforcePostCompactBudget_CapsAllToolResults(t *testing.T) {
	c := newCompactorFor(&fakeSummarizer{})
	huge := strings.Repeat("y", 50_000) // 10× the cap
	msgs := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Grep"),
		toolResultMsg("t1", huge),
		toolUseMsg("t2", "Bash"),
		toolResultMsg("t2", huge),
		msg(llm.RoleAssistant, "ok"),
	}
	out := c.EnforcePostCompactBudget(msgs)
	for i, m := range out {
		for _, b := range m.Content {
			if b.Type != "tool_result" {
				continue
			}
			// 5000 head + ~50 marker chars = ~5050 cap total
			if len(b.ToolResult) > PostCompactMaxToolResultChars+200 {
				t.Errorf("msg[%d] tool_result not clamped: len=%d", i, len(b.ToolResult))
			}
			if !strings.Contains(b.ToolResult, "[truncated post-compact:") {
				t.Errorf("msg[%d] tool_result missing post-compact marker", i)
			}
		}
	}
}

func TestEnforcePostCompactBudget_NoOpWhenAllUnderCap(t *testing.T) {
	c := newCompactorFor(&fakeSummarizer{})
	short := strings.Repeat("z", 500)
	msgs := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Grep"),
		toolResultMsg("t1", short),
		msg(llm.RoleAssistant, "ok"),
	}
	out := c.EnforcePostCompactBudget(msgs)
	// No mutations expected — should return the same backing slice.
	if !sameSlice(out, msgs) {
		t.Errorf("EnforcePostCompactBudget should return the input slice when nothing was over the cap")
	}
}

// TestSnipAll_AlsoSnipsProtectedTail — SnipAll is the recovery-path
// variant that ignores ProtectLast. The protected-tail invariant
// only matters for in-flight conversations; once a request has
// already bounced with overflow, preserving the tail's recoverability
// is meaningless (the model will never see those tool_results
// anyway), and snipping them is what rescues us from a single huge
// tail tool dump.
func TestSnipAll_AlsoSnipsProtectedTail(t *testing.T) {
	c := newCompactorFor(&fakeSummarizer{})
	long := strings.Repeat("z", 5000)
	msgs := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", long), // middle
		toolUseMsg("t2", "Bash"),
		toolResultMsg("t2", long), // protected by ProtectLast
		msg(llm.RoleAssistant, "final"),
	}
	out := c.SnipAll(msgs)
	// Both middle AND tail tool_results should be snipped.
	if len(out[2].Content[0].ToolResult) >= 5000 {
		t.Errorf("middle tool_result not snipped; len=%d", len(out[2].Content[0].ToolResult))
	}
	if len(out[4].Content[0].ToolResult) >= 5000 {
		t.Errorf("tail tool_result should ALSO be snipped by SnipAll; len=%d", len(out[4].Content[0].ToolResult))
	}
}

// TestSnip_NoOpWhenAllShort — when nothing exceeds the cap, Snip
// returns equivalent content (no spurious mutations).
func TestSnip_NoOpWhenAllShort(t *testing.T) {
	c := newCompactorFor(&fakeSummarizer{})
	msgs := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Read"),
		toolResultMsg("t1", "tiny"),
		msg(llm.RoleUser, "tail"),
		msg(llm.RoleAssistant, "ok"),
		msg(llm.RoleUser, "ok"),
	}
	out := c.Snip(msgs)
	if out[2].Content[0].ToolResult != "tiny" {
		t.Errorf("Snip should not modify under-cap results; got %q", out[2].Content[0].ToolResult)
	}
}

// TestSnip_DoesNotShareSlicesWithCaller — Snip mutates a local copy so
// callers that still hold the input slice don't see the truncation.
// Without this, concurrent History() snapshots would observe a
// half-snipped state.
func TestSnip_DoesNotShareSlicesWithCaller(t *testing.T) {
	c := newCompactorFor(&fakeSummarizer{})
	long := strings.Repeat("w", 5000)
	msgs := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", long),
		msg(llm.RoleUser, "tail-1"),
		msg(llm.RoleAssistant, "tail-2"),
		msg(llm.RoleUser, "tail-3"),
	}
	originalLen := len(msgs[2].Content[0].ToolResult)
	_ = c.Snip(msgs)
	if len(msgs[2].Content[0].ToolResult) != originalLen {
		t.Errorf("input slice was mutated in place; original len %d → now %d", originalLen, len(msgs[2].Content[0].ToolResult))
	}
}

// TestShouldSnip_ThresholdGate — Snip fires earlier than full compact.
// With Threshold=0.85 and SnipThreshold=0.70 (defaults), a 75%-full
// context should ShouldSnip but not ShouldCompact.
func TestShouldSnip_ThresholdGate(t *testing.T) {
	c := newCompactorFor(&fakeSummarizer{})
	// effectiveInputCap = 1000 - 0 = 1000 (MaxOutputTokens=0 in helper).
	// Need ~75% = ~750 tokens. estimateTokens charges 4/msg + 8/block + chars/4.
	// 750 tokens ≈ 3000 chars in a single text message.
	msgs := []llm.Message{msg(llm.RoleUser, strings.Repeat("x", 3000))}
	if !c.ShouldSnip(msgs) {
		t.Errorf("ShouldSnip should fire at ~75%%; estimateTokens=%d, cap=%d, snipThresh=%v",
			estimateTokens(msgs), c.effectiveInputCap(), c.SnipThreshold)
	}
	if c.ShouldCompact(msgs) {
		t.Errorf("ShouldCompact should NOT fire at ~75%% (Threshold=0.85)")
	}
}

// TestShouldSnip_DisabledWhenThresholdZero — caller-supplied
// SnipThreshold=0 means "snip disabled"; never returns true.
func TestShouldSnip_DisabledWhenThresholdZero(t *testing.T) {
	c := newCompactorFor(&fakeSummarizer{})
	c.SnipThreshold = 0
	huge := []llm.Message{msg(llm.RoleUser, strings.Repeat("x", 100000))}
	if c.ShouldSnip(huge) {
		t.Errorf("ShouldSnip must respect zero-threshold (disabled) sentinel")
	}
}

// TestCompactor_CircuitBreaker_NoOpDoesNotCount — Compact() returning
// "messages, nil" because the convo is too small (or already empty) is
// not a failure. Without this guard, an idle session would burn through
// the breaker budget before any real attempt.
func TestCompactor_CircuitBreaker_NoOpDoesNotCount(t *testing.T) {
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	tiny := []llm.Message{msg(llm.RoleUser, "hi")}

	for i := 0; i < 5; i++ {
		_, _ = c.Compact(context.Background(), tiny)
	}
	if c.consecutiveFailures != 0 {
		t.Errorf("no-op short-circuit (too-small convo) must not count toward breaker; got %d", c.consecutiveFailures)
	}
}

// TestShouldCompact_MinimumTokensFloor_Guards — proves the
// DeepSeek-TUI-style absolute floor refuses to fire on a small
// session even when the percentage threshold is technically crossed.
//
// Without this guard, an aggressive Threshold (0.95) on a tiny
// max-context configuration (1000 in test) would still rewrite
// prompt-cache anchors over what is effectively a few-turn convo —
// the user-visible symptom is "metis runs Compact 3 times in the
// first 10 turns and the cache hit rate craters." With the floor,
// Compact waits until there's actually enough material to summarize.
func TestShouldCompact_MinimumTokensFloor_Guards(t *testing.T) {
	p := &fakeSummarizer{}
	c := NewCompactor(Config{Threshold: 0.5, MinimumTokens: 10_000}, "m", 200, p)

	// 800 chars → ~200 tokens. Above the percent threshold
	// (200*0.5=100) but FAR below MinimumTokens=10000. Must NOT
	// trigger Compact — the floor wins.
	convo := []llm.Message{msg(llm.RoleUser, "x")}
	for i := 0; i < 4; i++ {
		convo = append(convo, msg(llm.RoleUser, "y"))
	}
	if c.ShouldCompact(convo) {
		t.Errorf("MinimumTokens=10000 should suppress Compact when estimated tokens are well below floor; convo estimate=%d", estimateTokens(convo))
	}
}

// TestShouldCompact_MinimumTokensZero_LegacyBehaviour — opt-out path.
// MinimumTokens=0 returns metis to pre-2026-05-16 percent-only
// triggering, which is what the existing unit-test suite relies on.
func TestShouldCompact_MinimumTokensZero_LegacyBehaviour(t *testing.T) {
	p := &fakeSummarizer{}
	c := NewCompactor(Config{Threshold: 0.5, MinimumTokens: 0}, "m", 200, p)

	long := []llm.Message{msg(llm.RoleUser, strings.Repeat("x", 800))} // ~250 tokens
	if !c.ShouldCompact(long) {
		t.Errorf("MinimumTokens=0 (disabled) must let percent threshold fire; estimate=%d, threshold=%d", estimateTokens(long), int(float64(200)*0.5))
	}
}

// TestDefaultCompactionConfig_KeepsMinimumTokensZero — package-level
// default must stay 0 so unit tests using DefaultCompactionConfig()
// see legacy percent-only behaviour. The 50_000 floor is opt-in via
// runtime wiring (cfg.Session.AutoCompactMinimumTokens). Documents
// the deliberate split — without this test someone might "tidy up"
// the default to 50_000 and silently break a dozen existing tests.
func TestDefaultCompactionConfig_KeepsMinimumTokensZero(t *testing.T) {
	cfg := DefaultCompactionConfig()
	if cfg.MinimumTokens != 0 {
		t.Errorf("DefaultCompactionConfig().MinimumTokens = %d, want 0 (package-level stays disabled; runtime/agent_loop.go injects production value from cfg.Session.AutoCompactMinimumTokens)", cfg.MinimumTokens)
	}
	if cfg.Threshold != 0.95 {
		t.Errorf("DefaultCompactionConfig().Threshold = %v, want 0.95 (raised from 0.85 on 2026-05-16)", cfg.Threshold)
	}
}
