package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

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
	// Unified retention is token/semantic based rather than an exact fixed
	// count. It must reduce the transcript, begin with a synthetic user
	// checkpoint followed by an assistant summary, and preserve the active
	// request verbatim either in the tail or deterministic anchor.
	if len(out) >= len(msgs) {
		t.Fatalf("expected history reduction, got %d -> %d", len(msgs), len(out))
	}
	if out[1].Role != llm.RoleAssistant {
		t.Errorf("boundary should be assistant role (mid-array system rejected by APIs), got %q", out[1].Role)
	}
	if len(out[1].Content) == 0 || !strings.Contains(out[1].Content[0].Text, "MOCK_SUMMARY") {
		t.Errorf("boundary missing summary, got: %v", out[1].Content)
	}
	if !historyHasText(out, "old user 5") {
		t.Errorf("active-task anchor old user 5 was lost: %#v", out)
	}
}

func TestCompact_DropsRetainedImageWhenCheckpointWouldRemainOverBudget(t *testing.T) {
	p := &fakeSummarizer{}
	cfg := DefaultCompactionConfig()
	cfg.RetainMinMessages = 3
	cfg.RetainMinUserMessages = 1
	c := NewCompactor(cfg, "large-window", 1_000_000, p)

	msgs := []llm.Message{msg(llm.RoleUser, "old request")}
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			msg(llm.RoleAssistant, "old assistant "+istr(i)),
			msg(llm.RoleUser, "old user "+istr(i)),
		)
	}
	msgs = append(msgs, llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Type: "text", Text: "inspect this screenshot"},
			{Type: "image", MediaType: "image/png", Data: strings.Repeat("A", 100_000)},
		},
	})

	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if got := estimateTokens(out); got >= PostCompactTokenBudget {
		t.Fatalf("checkpoint still exceeds post-compact budget: %d tokens", got)
	}
	for _, m := range out {
		for _, block := range m.Content {
			if block.Type == "image" || block.Data != "" {
				t.Fatal("oversized checkpoint retained raw image payload")
			}
			if strings.Contains(block.Text, "image cleared by compactor") && !strings.Contains(block.Text, "0 most recent screenshots kept") {
				t.Fatalf("full-checkpoint sentinel does not explain that images were cleared: %q", block.Text)
			}
		}
	}
	if !historyHasText(out, "image cleared by compactor") {
		t.Fatal("checkpoint removed image payload without leaving a reattach sentinel")
	}
}

func TestCompactWithInstructions_IsRequestLocalAndDelimited(t *testing.T) {
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	msgs := []llm.Message{msg(llm.RoleSystem, "sys")}
	for i := 0; i < 6; i++ {
		msgs = append(msgs,
			msg(llm.RoleUser, "old user "+istr(i)),
			msg(llm.RoleAssistant, "old assistant "+istr(i)),
		)
	}
	msgs = append(msgs,
		msg(llm.RoleUser, "recent user"),
		msg(llm.RoleAssistant, "recent assistant"),
		msg(llm.RoleUser, "active task"),
	)

	if _, err := c.CompactWithInstructions(context.Background(), msgs, " Preserve exact file paths and open questions. "); err != nil {
		t.Fatal(err)
	}
	firstPrompt := p.lastReq.Messages[0].Content[0].Text
	if !strings.Contains(firstPrompt, "<compact_instructions>\nPreserve exact file paths and open questions.\n</compact_instructions>") {
		t.Fatalf("custom instructions were not delimited in summarizer prompt:\n%s", firstPrompt)
	}

	if _, err := c.Compact(context.Background(), msgs); err != nil {
		t.Fatal(err)
	}
	secondPrompt := p.lastReq.Messages[0].Content[0].Text
	if strings.Contains(secondPrompt, "Preserve exact file paths") || strings.Contains(secondPrompt, "<compact_instructions>") {
		t.Fatalf("manual compact instructions leaked into the next ordinary Compact:\n%s", secondPrompt)
	}
}

func TestNormalizeCompactInstructions_BoundsRunesWithoutBreakingUTF8(t *testing.T) {
	in := "HEAD" + strings.Repeat("界", maxCompactInstructionRunes+10) + "TAIL"
	got := normalizeCompactInstructions(in)
	if !strings.Contains(got, "[additional compact instructions truncated]") {
		t.Fatal("long instructions were not marked truncated")
	}
	marker := "\n[additional compact instructions truncated]\n"
	parts := strings.Split(got, marker)
	if len(parts) != 2 {
		t.Fatalf("normalized instructions do not contain exactly one marker: %q", got)
	}
	if contentRunes := len([]rune(parts[0])) + len([]rune(parts[1])); contentRunes != maxCompactInstructionRunes {
		t.Fatalf("bounded content runes = %d, want %d", contentRunes, maxCompactInstructionRunes)
	}
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") {
		t.Fatalf("normalization did not preserve both instruction ends")
	}
	if !utf8.ValidString(got) {
		t.Fatal("normalization produced invalid UTF-8")
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
	assertBalancedToolPairs(t, out)
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

// TestShouldCompact_AccountsForMaxTokens — repro of the user's MiniMax-M2.7
// case in miniature: context window 1000, max_tokens 400, threshold 0.85.
//
// The trigger uses the effective input budget after reserving the configured
// response allowance. This prevents the request from overflowing before a
// nominal percentage of MaxContextTokens is reached.
func TestShouldCompact_AccountsForMaxTokens(t *testing.T) {
	p := &fakeSummarizer{}
	c := NewCompactor(Config{Threshold: 0.85}, "m", 1000, p)
	c.MaxOutputTokens = 400

	below := []llm.Message{msg(llm.RoleUser, strings.Repeat("x", 1200))}
	if c.ShouldCompact(below) {
		t.Errorf("~300 tokens should be below effective trigger %d", c.TriggerTokens())
	}

	above := []llm.Message{msg(llm.RoleUser, strings.Repeat("x", 3000))}
	if !c.ShouldCompact(above) {
		t.Errorf("~750 tokens should be above effective trigger %d", c.TriggerTokens())
	}

	// Removing the output reservation raises the trigger back to 850.
	c.MaxOutputTokens = 0
	if c.ShouldCompact(below) {
		t.Errorf("MaxOutputTokens=0: small history should remain below trigger %d", c.TriggerTokens())
	}
}

// TestEffectiveInputCapRejectsInvalidReservation covers the degenerate config:
// max_tokens accidentally set bigger than context_window. Returning the full
// window would hide an invalid request and allow an overflow.
func TestEffectiveInputCap_DefaultsSafely(t *testing.T) {
	c := &Compactor{
		MaxContextTokens: 1000,
		MaxOutputTokens:  2000, // user mis-config
	}
	if got := c.effectiveInputCap(); got != 0 {
		t.Errorf("oversized MaxOutputTokens should produce no usable input cap; got %d", got)
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
	if cfg.Threshold != 0.85 {
		t.Errorf("DefaultCompactionConfig().Threshold = %v, want 0.85 (unified heavy checkpoint trigger)", cfg.Threshold)
	}
}

// TestDefaultCompactionConfig_SetsMaxSummarizeInputTokens — locks in
// the bounded default. 96K keeps prefill near 32s on a ~3K tok/s
// provider while leaving time to generate the checkpoint. Setting this to 0 would
// re-enable the "1M-context Compact eats the whole middle in one
// call" path that produced the 2026-07-26 "compaction stuck at
// 950K tokens" report.
func TestDefaultCompactionConfig_SetsMaxSummarizeInputTokens(t *testing.T) {
	cfg := DefaultCompactionConfig()
	if cfg.MaxSummarizeInputTokens != 96_000 {
		t.Errorf("DefaultCompactionConfig().MaxSummarizeInputTokens = %d, want 96_000", cfg.MaxSummarizeInputTokens)
	}
	if cfg.MaxSummaryTokens != 8_192 {
		t.Errorf("DefaultCompactionConfig().MaxSummaryTokens = %d, want 8192", cfg.MaxSummaryTokens)
	}
	if cfg.SummaryTimeoutSeconds != 180 {
		t.Errorf("DefaultCompactionConfig().SummaryTimeoutSeconds = %d, want 180", cfg.SummaryTimeoutSeconds)
	}
	if cfg.MaxSummaryRetries != 1 {
		t.Errorf("DefaultCompactionConfig().MaxSummaryRetries = %d, want 1", cfg.MaxSummaryRetries)
	}
}

func TestSummaryWireLimitsShrinkForSmallModelWindow(t *testing.T) {
	tests := []struct {
		name       string
		window     int
		wantInput  int
		wantOutput int
	}{
		{name: "unknown", window: 0, wantInput: 96_000, wantOutput: 8_192},
		{name: "one-token", window: 1, wantInput: 1, wantOutput: 0},
		{name: "2k", window: 2_048, wantInput: 1_434, wantOutput: 512},
		{name: "below-4k", window: 4_095, wantInput: 2_868, wantOutput: 1_023},
		{name: "4k", window: 4_096, wantInput: 2_868, wantOutput: 1_024},
		{name: "16k", window: 16_000, wantInput: 11_488, wantOutput: 4_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultCompactionConfig()
			c := NewCompactor(cfg, "small-window", tt.window, &fakeSummarizer{})
			input, output := c.summaryWireLimits()
			if input != tt.wantInput || output != tt.wantOutput {
				t.Fatalf("summary limits = (%d, %d), want (%d, %d)", input, output, tt.wantInput, tt.wantOutput)
			}
			if tt.window > 0 {
				safety := tt.window / 20
				if safety > maxSummarySafetyTokens {
					safety = maxSummarySafetyTokens
				}
				if input+output+safety > tt.window {
					t.Fatalf("summary limits exceed window: input=%d output=%d safety=%d window=%d", input, output, safety, tt.window)
				}
			}
		})
	}
}

func TestSummarizeRequestFitsTwoKModelWindow(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.SummaryTimeoutSeconds = 0
	p := &fakeSummarizer{}
	c := NewCompactor(cfg, "tiny-window", 2_048, p)
	messages := []llm.Message{
		msg(llm.RoleUser, strings.Repeat("large transcript ", 20_000)),
		msg(llm.RoleAssistant, strings.Repeat("large result ", 20_000)),
	}
	if _, err := c.summarizeWithInstructions(context.Background(), messages, "", ""); err != nil {
		t.Fatalf("summarizeWithInstructions: %v", err)
	}
	inputCap, outputCap := c.summaryWireLimits()
	fed := estimateStringTokens(p.lastReq.System) + estimateTokens(p.lastReq.Messages)
	if fed > inputCap {
		t.Fatalf("summary request input = %d, cap %d", fed, inputCap)
	}
	if p.lastReq.MaxTokens != outputCap {
		t.Fatalf("summary max_tokens = %d, want %d", p.lastReq.MaxTokens, outputCap)
	}
	const safety = 2_048 / 20
	if fed+p.lastReq.MaxTokens+safety > 2_048 {
		t.Fatalf("summary request exceeds window: input=%d output=%d safety=%d", fed, p.lastReq.MaxTokens, safety)
	}
}

func TestCompactHardFitsTwoKAndFourKWindows(t *testing.T) {
	for _, window := range []int{2_048, 4_096} {
		t.Run(fmt.Sprintf("%d", window), func(t *testing.T) {
			cfg := DefaultCompactionConfig()
			cfg.SummaryTimeoutSeconds = 0
			provider := &fakeSummarizer{}
			compactor := NewCompactor(cfg, "small-window", window, provider)
			messages := make([]llm.Message, 0, 15)
			for i := 0; i < 15; i++ {
				messages = append(messages, msg(llm.RoleUser,
					fmt.Sprintf("request-%02d %s", i, strings.Repeat("x", 4_000))))
			}

			out, err := compactor.Compact(context.Background(), messages)
			if err != nil {
				t.Fatalf("Compact: %v", err)
			}
			historyCap, constrained, err := compactor.postCompactHistoryCap(0)
			if err != nil || !constrained {
				t.Fatalf("postCompactHistoryCap: constrained=%v err=%v", constrained, err)
			}
			if got := estimateTokens(out); got >= historyCap {
				t.Fatalf("compact installed %d tokens into exclusive history cap %d", got, historyCap)
			}
			if provider.calls != 1 {
				t.Fatalf("summary calls=%d, want one deterministic request", provider.calls)
			}
		})
	}
}

func TestSummarizeRejectsZeroOutputBudgetBeforeProviderCall(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.SummaryTimeoutSeconds = 0
	provider := &fakeSummarizer{}
	compactor := NewCompactor(cfg, "one-token", 1, provider)
	_, err := compactor.summarizeWithInstructions(context.Background(), []llm.Message{
		msg(llm.RoleUser, "hello"),
	}, "", "")
	if err == nil || !strings.Contains(err.Error(), "no positive output budget") {
		t.Fatalf("zero output budget should fail clearly, got %v", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider received MaxTokens=0 request: calls=%d", provider.calls)
	}
}

func TestSummarizeRequestFitsActualSmallModelWindow(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.SummaryTimeoutSeconds = 0
	p := &fakeSummarizer{}
	c := NewCompactor(cfg, "small-window", 10_000, p)
	messages := []llm.Message{
		msg(llm.RoleUser, strings.Repeat("large transcript ", 20_000)),
		msg(llm.RoleAssistant, strings.Repeat("large result ", 20_000)),
	}
	if _, err := c.summarizeWithInstructions(context.Background(), messages, "", ""); err != nil {
		t.Fatalf("summarizeWithInstructions: %v", err)
	}
	inputCap, outputCap := c.summaryWireLimits()
	fed := estimateStringTokens(p.lastReq.System) + estimateTokens(p.lastReq.Messages)
	if fed > inputCap {
		t.Fatalf("summary request input = %d, cap %d", fed, inputCap)
	}
	if p.lastReq.MaxTokens != outputCap {
		t.Fatalf("summary max_tokens = %d, want %d", p.lastReq.MaxTokens, outputCap)
	}
	if fed+p.lastReq.MaxTokens+10_000/20 > 10_000 {
		t.Fatalf("summary request exceeds window: input=%d output=%d safety=%d", fed, p.lastReq.MaxTokens, 10_000/20)
	}
}

// TestCompact_RespectsMaxSummarizeInputTokens — when the pending
// middle slice exceeds MaxSummarizeInputTokens, Compact must fit the
// transcript locally and make exactly one provider request. This locks out
// the old up-to-eight CollapseMiddle request chain.
func TestCompact_RespectsMaxSummarizeInputTokens(t *testing.T) {
	// Build a long conversation: system + many user/assistant text
	// pairs, each ~1000 estimated tokens (4000 ASCII chars / 4).
	big := strings.Repeat("x", 4000)
	msgs := []llm.Message{msg(llm.RoleSystem, "sys")}
	for i := 0; i < 30; i++ {
		msgs = append(msgs, msg(llm.RoleUser, big))
		msgs = append(msgs, msg(llm.RoleAssistant, big))
	}
	// Total ≈ 1 + 60 messages, each ~1000 tokens → ~60K middle.
	// Cap the summarize input at 5K so Compact must select evidence from
	// the oversized history before making its one summary request.

	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 5
	cfg.CollapseFoldWindow = 10
	cfg.MaxSummarizeInputTokens = 5_000

	p := &fakeSummarizer{}
	c := NewCompactor(cfg, "test", 100_000, p)
	c.Provider = p

	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(out) >= len(msgs) {
		t.Errorf("Compact returned no progress: in=%d out=%d", len(msgs), len(out))
	}
	// The final summarize() call must have received a middle that
	// fits the budget. Inspect the recorded request: the prompt's
	// message list is what was fed to summarize(). We assert the
	// ESTIMATED size of that prompt is ≤ MaxSummarizeInputTokens
	// plus a generous slack for the system prompt + iterative seed
	// text (which aren't part of the budget but do land in the
	// request).
	if p.calls != 1 {
		t.Fatalf("summarize calls = %d, want exactly 1", p.calls)
	}
	lastReq := p.lastReq
	var fedMessages []llm.Message
	for _, m := range lastReq.Messages {
		fedMessages = append(fedMessages, m)
	}
	fed := estimateStringTokens(lastReq.System) + estimateTokens(fedMessages)
	if fed > cfg.MaxSummarizeInputTokens {
		t.Errorf("final summarize() wire prompt estimate = %d tokens, want ≤ %d",
			fed, cfg.MaxSummarizeInputTokens)
	}
}

func TestBuildSummaryTranscript_PreservesAnchorsUsersAndNewestEvidence(t *testing.T) {
	msgs := []llm.Message{msg(llm.RoleAssistant, "EARLIEST_ANCHOR "+strings.Repeat("h", 4_000))}
	for i := 0; i < 24; i++ {
		msgs = append(msgs,
			msg(llm.RoleAssistant, strings.Repeat("old assistant evidence ", 400)),
			msg(llm.RoleUser, "older request "+istr(i)+" "+strings.Repeat("u", 1_000)),
		)
	}
	msgs = append(msgs,
		msg(llm.RoleUser, "CRITICAL_USER_REQUEST "+strings.Repeat("c", 2_000)),
		msg(llm.RoleAssistant, "LATEST_EVIDENCE "+strings.Repeat("z", 8_000)),
	)

	const budget = 1_200
	got := buildSummaryTranscript(msgs, false, budget)
	if tokens := estimateStringTokens(got); tokens > budget {
		t.Fatalf("fitted transcript = %d tokens, budget %d", tokens, budget)
	}
	for _, want := range []string{"Transcript locally fitted", "EARLIEST_ANCHOR", "CRITICAL_USER_REQUEST", "LATEST_EVIDENCE"} {
		if !strings.Contains(got, want) {
			t.Errorf("fitted transcript lost %q", want)
		}
	}
}

func TestBuildSummaryTranscript_ReservesToolAndErrorEvidence(t *testing.T) {
	failed := toolResultMsg("critical-call", "CRITICAL_TOOL_ERROR "+strings.Repeat("failure details ", 1_000))
	failed.Content[0].IsError = true
	msgs := []llm.Message{
		msg(llm.RoleUser, "original task"),
		toolUseMsg("critical-call", "BuildVerifier"),
		failed,
	}
	for i := 0; i < 40; i++ {
		msgs = append(msgs, msg(llm.RoleAssistant, strings.Repeat("routine success output ", 500)))
	}
	msgs = append(msgs, msg(llm.RoleAssistant, "LATEST_RESULT "+strings.Repeat("z", 4_000)))

	got := buildSummaryTranscript(msgs, false, 1_500)
	for _, want := range []string{"BuildVerifier", "critical-call", "CRITICAL_TOOL_ERROR", "LATEST_RESULT"} {
		if !strings.Contains(got, want) {
			t.Errorf("fitted transcript lost reserved evidence %q", want)
		}
	}
}

func TestBuildSummaryTranscript_ReservesSuccessfulToolTransaction(t *testing.T) {
	use := toolUseMsg("verified-build", "Bash")
	use.Content[1].ToolInput = map[string]any{"command": "go test ./..."}
	result := toolResultMsg(
		"verified-build",
		"VERIFIED_BUILD_HEAD\n"+strings.Repeat("successful compiler output ", 2_000)+"\nVERIFIED_BUILD_TAIL exit=0",
	)
	msgs := []llm.Message{
		msg(llm.RoleAssistant, "ANCIENT_BULK "+strings.Repeat("old output ", 8_000)),
		msg(llm.RoleUser, "fix the compaction implementation"),
		use,
		result,
		msg(llm.RoleAssistant, "unrelated middle status"),
		msg(llm.RoleAssistant, "LATEST_EVIDENCE "+strings.Repeat("newest output ", 8_000)),
	}

	got := buildSummaryTranscript(msgs, false, 1_200)
	if tokens := estimateStringTokens(got); tokens > 1_200 {
		t.Fatalf("fitted transcript = %d tokens, budget 1200", tokens)
	}
	for _, want := range []string{
		"verified-build",
		"go test ./...",
		"VERIFIED_BUILD_HEAD",
		"VERIFIED_BUILD_TAIL exit=0",
		"omitted",
		"transcript evidence",
		"LATEST_EVIDENCE",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("fitted transcript lost successful transaction evidence %q:\n%s", want, got)
		}
	}
}

func TestBuildSummaryTranscript_ReservesParallelSuccessfulToolTransactions(t *testing.T) {
	parallelUses := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUseID: "build-call", ToolName: "Bash", ToolInput: map[string]any{"command": "go build ./..."}},
			{Type: "tool_use", ToolUseID: "vet-call", ToolName: "Bash", ToolInput: map[string]any{"command": "go vet ./..."}},
			{Type: "tool_use", ToolUseID: "test-call", ToolName: "Bash", ToolInput: map[string]any{"command": "go test -race ./..."}},
		},
	}
	// Providers return parallel tool results in one user message. Make each
	// result large enough that fitting the whole message head+tail would retain
	// the first and last results while silently erasing the middle transaction.
	parallelResults := llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{
				Type:       "tool_result",
				ToolUseID:  "build-call",
				ToolResult: "BUILD_RESULT_HEAD\n" + strings.Repeat("build success output ", 3_000) + "\nBUILD_RESULT_TAIL exit=0",
			},
			{
				Type:       "tool_result",
				ToolUseID:  "vet-call",
				ToolResult: "VET_RESULT_HEAD\n" + strings.Repeat("vet success output ", 3_000) + "\nVET_RESULT_TAIL exit=0",
			},
			{
				Type:       "tool_result",
				ToolUseID:  "test-call",
				ToolResult: "TEST_RESULT_HEAD\n" + strings.Repeat("test success output ", 3_000) + "\nTEST_RESULT_TAIL exit=0",
			},
		},
	}
	msgs := []llm.Message{
		msg(llm.RoleAssistant, "ANCIENT_BULK "+strings.Repeat("old output ", 8_000)),
		msg(llm.RoleUser, "verify build, vet, and race tests"),
		parallelUses,
		parallelResults,
		msg(llm.RoleAssistant, "LATEST_BULK "+strings.Repeat("bulk ", 20_000)),
	}

	got := buildSummaryTranscript(msgs, false, 1_500)
	if tokens := estimateStringTokens(got); tokens > 1_500 {
		t.Fatalf("fitted transcript = %d tokens, budget 1500", tokens)
	}
	var missing []string
	for _, want := range []string{
		"build-call",
		"go build ./...",
		"BUILD_RESULT_HEAD",
		"BUILD_RESULT_TAIL exit=0",
		"vet-call",
		"go vet ./...",
		"VET_RESULT_HEAD",
		"VET_RESULT_TAIL exit=0",
		"test-call",
		"go test -race ./...",
		"TEST_RESULT_HEAD",
		"TEST_RESULT_TAIL exit=0",
	} {
		if !strings.Contains(got, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("parallel successful transactions were not retained as units; missing %q:\n%s", missing, got)
	}
}

func TestBuildSummaryTranscript_TinyBudgetKeepsSuccessfulTransactionAtomic(t *testing.T) {
	use := toolUseMsg("tiny-atomic", "Bash")
	use.Content[1].ToolInput = map[string]any{"command": "TINY_ATOMIC_COMMAND"}
	msgs := []llm.Message{
		msg(llm.RoleAssistant, "ANCIENT_BULK "+strings.Repeat("old output ", 4_000)),
		msg(llm.RoleUser, "keep the successful transaction atomic"),
		use,
		toolResultMsg(
			"tiny-atomic",
			"TINY_ATOMIC_RESULT_HEAD\n"+strings.Repeat("successful result detail ", 2_000)+"\nTINY_ATOMIC_RESULT_TAIL exit=0",
		),
	}

	const budget = 160
	got := buildSummaryTranscript(msgs, false, budget)
	if tokens := estimateStringTokens(got); tokens > budget {
		t.Fatalf("fitted transcript = %d tokens, budget %d", tokens, budget)
	}
	hasUse := strings.Contains(got, `ASSISTANT tool_use id="tiny-atomic"`)
	hasResult := strings.Contains(got, `USER tool_result id="tiny-atomic"`)
	if hasUse != hasResult {
		t.Fatalf("successful transaction was split at tiny budget: has use=%t result=%t\n%s", hasUse, hasResult, got)
	}
	if !hasUse && !strings.Contains(got, "omitted tool transactions success=1") {
		t.Fatalf("atomic omission was not reported explicitly:\n%s", got)
	}
}

func TestFitSummaryTranscriptSegment_TransactionSkeletonIsAllOrNone(t *testing.T) {
	txn := summaryToolTransaction{
		key: "id:boundary-atomic", id: "boundary-atomic", latest: 1,
		toolName: "Bash", hasUse: true, hasResult: true,
		atoms: []summaryToolAtom{
			{
				order: 0, kind: "tool_use",
				text:    "ASSISTANT tool_use id=\"boundary-atomic\" name=\"Bash\" input={\"command\":\"go test ./...\"}\n",
				minimum: "ASSISTANT tool_use id=\"boundary-atomic\" name=\"Bash\" input={\"command\":\"go test ./...\"}\n",
			},
			{
				order: 1, kind: "tool_result",
				text:    "USER tool_result id=\"boundary-atomic\" is_error=false: PASS exit=0\n",
				minimum: "USER tool_result id=\"boundary-atomic\" is_error=false: PASS exit=0\n",
			},
		},
	}
	seg := buildSummaryToolTransaction(txn)
	minimumCost := estimateStringTokens(seg.minimum)
	if minimumCost < 2 {
		t.Fatalf("unexpected transaction skeleton cost %d", minimumCost)
	}
	if got := fitSummaryTranscriptSegment(seg, minimumCost-1); got != "" {
		t.Fatalf("budget below skeleton retained a partial transaction:\n%s", got)
	}
	got := fitSummaryTranscriptSegment(seg, minimumCost)
	for _, want := range []string{`tool_use id="boundary-atomic"`, `tool_result id="boundary-atomic"`, "PASS exit=0"} {
		if !strings.Contains(got, want) {
			t.Fatalf("exact skeleton budget lost %q:\n%s", want, got)
		}
	}
}

func TestBuildSummaryTranscript_ManyGapMarkersDoNotEraseSuccessfulTransaction(t *testing.T) {
	msgs := []llm.Message{
		msg(llm.RoleAssistant, "MANY_GAPS_EARLIEST "+strings.Repeat("anchor ", 2_000)),
	}
	for i := 0; i < 80; i++ {
		msgs = append(msgs,
			msg(llm.RoleAssistant, "UNSELECTED_FILLER_"+istr(i)+" "+strings.Repeat("bulk ", 2_000)),
		)
		if i == 40 {
			use := toolUseMsg("many-gaps-success", "Bash")
			use.Content[1].ToolInput = map[string]any{"command": "MANY_GAPS_SUCCESS_COMMAND"}
			msgs = append(msgs,
				use,
				toolResultMsg("many-gaps-success", "MANY_GAPS_SUCCESS_RESULT exit=0"),
			)
		}
		// These short, semantically reserved user messages are deliberately
		// non-contiguous, so rendering the selected set needs many gap markers.
		msgs = append(msgs, msg(llm.RoleUser, fmt.Sprintf("user-%03d", i)))
	}
	msgs = append(msgs, msg(llm.RoleAssistant, "MANY_GAPS_LATEST "+strings.Repeat("latest ", 2_000)))

	const budget = 1_000
	got := buildSummaryTranscript(msgs, false, budget)
	if tokens := estimateStringTokens(got); tokens > budget {
		t.Fatalf("fitted transcript = %d tokens, budget %d", tokens, budget)
	}
	missing := make([]string, 0, 4)
	for _, want := range []string{
		`ASSISTANT tool_use id="many-gaps-success"`,
		"MANY_GAPS_SUCCESS_COMMAND",
		`USER tool_result id="many-gaps-success"`,
		"MANY_GAPS_SUCCESS_RESULT exit=0",
	} {
		if !strings.Contains(got, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("gap-marker fitting erased successful transaction evidence %q:\n%s", missing, got)
	}
}

func TestBuildSummaryTranscript_BudgetsSuccessfulAndFailedTransactionsIndependently(t *testing.T) {
	failed := toolResultMsg("failed-check", "FAILED_CHECK_EVIDENCE exit=1")
	failed.Content[0].IsError = true
	msgs := []llm.Message{
		msg(llm.RoleAssistant, "ANCIENT_BULK "+strings.Repeat("old output ", 8_000)),
		msg(llm.RoleUser, "preserve both positive and negative verification"),
		toolUseMsg("successful-check", "Bash"),
		toolResultMsg("successful-check", "SUCCESSFUL_CHECK_EVIDENCE exit=0"),
		toolUseMsg("failed-check", "Bash"),
		failed,
		msg(llm.RoleAssistant, "LATEST_BULK "+strings.Repeat("bulk ", 20_000)),
	}

	got := buildSummaryTranscript(msgs, false, 1_500)
	for _, want := range []string{
		"successful-check",
		"SUCCESSFUL_CHECK_EVIDENCE exit=0",
		"failed-check",
		"FAILED_CHECK_EVIDENCE exit=1",
		"is_error=true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("success/error evidence pools did not both survive; missing %q:\n%s", want, got)
		}
	}
}

func TestSummarizeInputBudgetIncludesSystemPriorAndInstructions(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.MaxSummarizeInputTokens = 3_000
	cfg.SummaryTimeoutSeconds = 0
	p := &fakeSummarizer{}
	c := NewCompactor(cfg, "test", 100_000, p)
	prior := "PRIOR_START " + strings.Repeat("prior context ", 8_000) + " PRIOR_END"
	instructions := normalizeCompactInstructions("INSTRUCTION_START " + strings.Repeat("be precise ", 2_000) + " INSTRUCTION_END")
	messages := []llm.Message{
		msg(llm.RoleUser, "important request "+strings.Repeat("u", 20_000)),
		msg(llm.RoleAssistant, "important result "+strings.Repeat("a", 20_000)),
	}
	if _, err := c.summarizeWithInstructions(context.Background(), messages, prior, instructions); err != nil {
		t.Fatalf("summarizeWithInstructions: %v", err)
	}
	fed := estimateStringTokens(p.lastReq.System) + estimateTokens(p.lastReq.Messages)
	if fed > cfg.MaxSummarizeInputTokens {
		t.Fatalf("system + prior + instructions + transcript = %d tokens, cap %d", fed, cfg.MaxSummarizeInputTokens)
	}
	payload := p.lastReq.Messages[0].Content[0].Text
	for _, want := range []string{"PRIOR_START", "PRIOR_END", "INSTRUCTION_START", "INSTRUCTION_END"} {
		if !strings.Contains(payload, want) {
			t.Errorf("budgeted prompt lost %q", want)
		}
	}
}

// TestCompact_TinyInputBudgetStillUsesOneSummaryRequest guards the extreme
// fitting path. CollapseFoldWindow must no longer affect the number of paid
// requests made by full compaction.
func TestCompact_TinyInputBudgetStillUsesOneSummaryRequest(t *testing.T) {
	big := strings.Repeat("y", 4000) // ~1000 tokens each
	// 1 system + 10 user/assistant pairs + 4 tail messages = 25 msgs.
	// ProtectFirst=1 + ProtectLast=5 → middle is 19 messages.
	// A historical CollapseFoldWindow large enough to consume the old middle
	// must not re-enable preliminary LLM folds.
	msgs := []llm.Message{msg(llm.RoleSystem, "sys")}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, msg(llm.RoleUser, big))
		msgs = append(msgs, msg(llm.RoleAssistant, big))
	}
	for i := 0; i < 4; i++ {
		msgs = append(msgs, msg(llm.RoleUser, "tail"))
	}

	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 5
	cfg.CollapseFoldWindow = 25
	cfg.MaxSummarizeInputTokens = 1_000 // tiny: forces deterministic local fitting

	p := &fakeSummarizer{}
	c := NewCompactor(cfg, "test", 100_000, p)
	c.Provider = p

	callsBefore := p.calls
	out, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(out) >= len(msgs) {
		t.Errorf("expected progress: in=%d out=%d", len(msgs), len(out))
	}
	if p.calls != callsBefore+1 {
		t.Errorf("summarize calls = %d, want %d (one fitted final summary)",
			p.calls, callsBefore+1)
	}
	if !strings.Contains(p.lastReq.Messages[0].Content[0].Text, "Transcript locally fitted") {
		t.Fatal("oversized summary prompt did not report deterministic local fitting")
	}
}
