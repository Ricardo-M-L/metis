package agent

// compact_v2_test.go — pins the Batch 1 compaction upgrades shipped
// 2026-05-13 after the cross-7-projects comparison:
//
//	#1 5-section summary prompt          (SummarySystemPromptInitial)
//	#2 iterative summary (prior-summary merge mode)
//	#4 protected tool whitelist (Snip / SnipAll / Microcompact skip)
//	#5 [REDACTED] secret scrubbing
//	#6 file-aware + CJK-aware token estimation
//	#8 summarize retry + non-stream fallback
//
// Test helpers (fakeStream, msg, toolUseMsg, toolResultMsg) live in
// compact_test.go and are reused here.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// --- helpers ---------------------------------------------------------------

// streamingProvider records the System prompt + last user payload then
// returns the configured response. Used by every iterative / prompt-shape
// assertion so tests can read back what the summarizer actually saw.
type streamingProvider struct {
	calls   int
	system  string
	payload string
	resp    string
}

func (s *streamingProvider) Name() string          { return "streaming-test" }
func (s *streamingProvider) MaxContextTokens() int { return 100_000 }
func (s *streamingProvider) ModelID() string       { return "" }
func (s *streamingProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	s.calls++
	s.system = req.System
	if len(req.Messages) > 0 && len(req.Messages[0].Content) > 0 {
		s.payload = req.Messages[0].Content[0].Text
	}
	resp := s.resp
	if resp == "" {
		resp = "MOCK_SUMMARY"
	}
	return &fakeStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: resp},
		{Type: "message_stop"},
	}}, nil
}
func (s *streamingProvider) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return nil, errors.New("compact_v2_test: streamingProvider.Complete called unexpectedly")
}

// flakyProvider fails its first `failsBeforeOK` Stream calls with the
// configured error, then succeeds. Lets the retry test observe both
// the retry counter and the eventual happy-path output.
type flakyProvider struct {
	failsBeforeOK int
	calls         int
	failErr       error
	resp          string
}

func (f *flakyProvider) Name() string          { return "flaky" }
func (f *flakyProvider) MaxContextTokens() int { return 100_000 }
func (f *flakyProvider) ModelID() string       { return "" }
func (f *flakyProvider) Stream(_ context.Context, _ llm.Request) (llm.StreamReader, error) {
	f.calls++
	if f.calls <= f.failsBeforeOK {
		err := f.failErr
		if err == nil {
			err = errors.New("flakyProvider: simulated transient error")
		}
		return nil, err
	}
	resp := f.resp
	if resp == "" {
		resp = "RETRY_OK_SUMMARY"
	}
	return &fakeStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: resp},
		{Type: "message_stop"},
	}}, nil
}
func (f *flakyProvider) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return nil, errors.New("flakyProvider: Complete called but stream eventually succeeded")
}

// alwaysFailStreamProvider fails Stream() every call (simulating a
// gateway whose SSE channel is permanently broken). Complete() returns
// a successful response so summarize's last-resort fallback path is
// exercised.
type alwaysFailStreamProvider struct {
	streamCalls   int
	completeCalls int
	completeText  string
}

func (a *alwaysFailStreamProvider) Name() string          { return "always-fail-stream" }
func (a *alwaysFailStreamProvider) MaxContextTokens() int { return 100_000 }
func (a *alwaysFailStreamProvider) ModelID() string       { return "" }
func (a *alwaysFailStreamProvider) Stream(_ context.Context, _ llm.Request) (llm.StreamReader, error) {
	a.streamCalls++
	return nil, errors.New("alwaysFailStreamProvider: SSE broken")
}
func (a *alwaysFailStreamProvider) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	a.completeCalls++
	text := a.completeText
	if text == "" {
		text = "FALLBACK_COMPLETE_SUMMARY"
	}
	return &llm.Response{
		Content: []llm.ContentBlock{{Type: "text", Text: text}},
	}, nil
}

func newCompactorForV2(p llm.Provider) *Compactor {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	return NewCompactor(cfg, "test-model", 10_000, p)
}

// longMiddle returns a slice big enough that Compact's
// "len <= ProtectFirst+ProtectLast+2" no-op guard doesn't trigger.
// Pads with neutral user/asst pairs that won't accidentally satisfy
// the active-task anchor (no chinese / no "task" verbs).
func longMiddle() []llm.Message {
	out := []llm.Message{msg(llm.RoleUser, "real user task: fix the auth bug")}
	for i := 0; i < 6; i++ {
		out = append(out, msg(llm.RoleAssistant, "ack and continued exploration"))
		out = append(out, msg(llm.RoleUser, "follow-up clarification"))
	}
	// trailing tail
	out = append(out, msg(llm.RoleAssistant, "applying fix"))
	out = append(out, msg(llm.RoleUser, "looks good"))
	out = append(out, msg(llm.RoleAssistant, "done"))
	return out
}

// --- #1: structured summary prompt (8 sections, CC-aligned 2026-06-13) ------

func TestSummary_InitialPromptHasAllSections(t *testing.T) {
	// 8 sections: the crush-5 plus the Claude-Code-aligned additions
	// (Primary Request & Intent, Errors & Fixes, Pending Tasks).
	want := []string{
		"## Primary Request & Intent",
		"## Current State",
		"## Files & Changes",
		"## Technical Context",
		"## Errors & Fixes",
		"## Pending Tasks",
		"## Strategy & Approach",
		"## Exact Next Steps",
	}
	for _, h := range want {
		if !strings.Contains(SummarySystemPromptInitial, h) {
			t.Errorf("SummarySystemPromptInitial missing %q", h)
		}
		if !strings.Contains(SummarySystemPromptMerge, h) {
			t.Errorf("SummarySystemPromptMerge missing %q", h)
		}
	}
	// Drift guard: Next Steps must instruct verbatim quoting of the
	// latest user ask (the CC anti-drift property).
	if !strings.Contains(SummarySystemPromptInitial, "VERBATIM") {
		t.Error("Next Steps should require quoting the latest user request verbatim (drift guard)")
	}
}

func TestSummary_SystemPromptPassedToProvider(t *testing.T) {
	p := &streamingProvider{}
	c := newCompactorForV2(p)
	all := longMiddle()
	if _, err := c.Compact(context.Background(), all); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !strings.Contains(p.system, "## Current State") {
		t.Errorf("provider didn't receive 5-section system prompt; got %q", truncate(p.system, 120))
	}
}

// --- #2: iterative summary -------------------------------------------------

func TestSummary_FirstCompactUsesInitialPrompt(t *testing.T) {
	p := &streamingProvider{}
	c := newCompactorForV2(p)
	if c.LastSummary != "" {
		t.Fatalf("LastSummary must start empty; got %q", c.LastSummary)
	}
	if _, err := c.Compact(context.Background(), longMiddle()); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !strings.HasPrefix(p.system, "You are summarizing an agent conversation") {
		t.Errorf("first compact didn't receive INITIAL system prompt; got %q", truncate(p.system, 80))
	}
	if c.LastSummary != "MOCK_SUMMARY" {
		t.Errorf("LastSummary not stashed after compact; got %q", c.LastSummary)
	}
}

func TestSummary_SecondCompactUsesMergeWhenPriorSummaryPresent(t *testing.T) {
	p := &streamingProvider{}
	c := newCompactorForV2(p)
	c.IterativeSummary = true
	// Seed prior summary state directly.
	c.LastSummary = "PRIOR_SUMMARY_BODY_v1"

	// Build a transcript where the middle's first message IS a prior
	// summary boundary — simulates the real second-compact shape.
	all := []llm.Message{msg(llm.RoleUser, "system seed")}
	all = append(all, llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{{
			Type: "text",
			Text: "[Earlier conversation summarized: PRIOR_SUMMARY_BODY_v1]",
		}},
	})
	for i := 0; i < 6; i++ {
		all = append(all, msg(llm.RoleUser, "new request post compact"))
		all = append(all, msg(llm.RoleAssistant, "answer "+strings.Repeat("x", 50)))
	}
	all = append(all, msg(llm.RoleAssistant, "tail1"))
	all = append(all, msg(llm.RoleUser, "tail2"))
	all = append(all, msg(llm.RoleAssistant, "tail3"))

	if _, err := c.Compact(context.Background(), all); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !strings.HasPrefix(p.system, "You are UPDATING") {
		t.Errorf("second compact didn't receive MERGE system prompt; got %q", truncate(p.system, 80))
	}
	if !strings.Contains(p.payload, "PRIOR_SUMMARY_BODY_v1") {
		t.Errorf("merge payload must include prior summary body verbatim; got %q", truncate(p.payload, 200))
	}
}

func TestSummary_DisableIterative_ForcesFresh(t *testing.T) {
	p := &streamingProvider{}
	c := newCompactorForV2(p)
	c.IterativeSummary = false
	c.LastSummary = "STALE_PRIOR"

	if _, err := c.Compact(context.Background(), longMiddle()); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if strings.HasPrefix(p.system, "You are UPDATING") {
		t.Errorf("IterativeSummary=false must use INITIAL prompt regardless of LastSummary; system=%q", truncate(p.system, 80))
	}
}

func TestResetCircuit_ClearsLastSummary(t *testing.T) {
	p := &streamingProvider{}
	c := newCompactorForV2(p)
	c.LastSummary = "stash"
	c.ResetCircuit()
	if c.LastSummary != "" {
		t.Errorf("ResetCircuit must clear LastSummary; got %q", c.LastSummary)
	}
}

// --- #4: protected tool whitelist ------------------------------------------

func TestSnip_ProtectsConfiguredToolByID(t *testing.T) {
	p := &streamingProvider{}
	c := newCompactorForV2(p)
	c.SnipMaxToolResultChars = 50
	c.ProtectedTools = []string{"memory_query"}
	huge := strings.Repeat("a", 800)

	msgs := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("u1", "memory_query"),
		toolResultMsg("u1", huge),
		toolUseMsg("u2", "Bash"),
		toolResultMsg("u2", huge),
		msg(llm.RoleAssistant, "tail1"),
		msg(llm.RoleUser, "tail2"),
		msg(llm.RoleAssistant, "tail3"),
	}
	got := c.Snip(msgs)

	// memory_query result must be untouched (still 800 chars, no
	// marker)
	mq := got[2].Content[0].ToolResult
	if len(mq) != 800 || strings.Contains(mq, "[snipped:") {
		t.Errorf("memory_query tool_result should be untouched; len=%d hasMarker=%v",
			len(mq), strings.Contains(mq, "[snipped:"))
	}
	// Bash result MUST be snipped
	bash := got[4].Content[0].ToolResult
	if !strings.Contains(bash, "[snipped:") {
		t.Errorf("Bash tool_result should be snipped; len=%d hasMarker=%v",
			len(bash), strings.Contains(bash, "[snipped:"))
	}
}

func TestSnipAll_AlsoRespectsProtectedTools(t *testing.T) {
	// SnipAll runs as an overflow rescue and crosses the protected
	// tail, but the structural protection (memory recall etc) still
	// applies — losing a memory_query result during a rescue
	// permanently breaks the model's recall even if the bounce
	// resolved.
	p := &streamingProvider{}
	c := newCompactorForV2(p)
	c.SnipMaxToolResultChars = 30
	c.ProtectedTools = []string{"Read"}
	body := strings.Repeat("x", 500)

	msgs := []llm.Message{
		msg(llm.RoleUser, "u"),
		toolUseMsg("u1", "Read"),
		toolResultMsg("u1", body),
		toolUseMsg("u2", "Bash"),
		toolResultMsg("u2", body),
	}
	got := c.SnipAll(msgs)
	if strings.Contains(got[2].Content[0].ToolResult, "[snipped:") {
		t.Errorf("SnipAll must not snip Read result; got %q",
			truncate(got[2].Content[0].ToolResult, 60))
	}
	if !strings.Contains(got[4].Content[0].ToolResult, "[snipped:") {
		t.Errorf("SnipAll must snip Bash result; got %q",
			truncate(got[4].Content[0].ToolResult, 60))
	}
}

func TestSnip_NoProtectionWhenWhitelistEmpty(t *testing.T) {
	// Empty ProtectedTools restores legacy "snip everything"
	// behaviour. Pinned so future maintainers don't accidentally
	// invert the default.
	p := &streamingProvider{}
	c := newCompactorForV2(p)
	c.SnipMaxToolResultChars = 30
	c.ProtectedTools = nil

	msgs := []llm.Message{
		msg(llm.RoleUser, "u"),
		toolUseMsg("u1", "memory_query"),
		toolResultMsg("u1", strings.Repeat("y", 500)),
		msg(llm.RoleAssistant, "tail1"),
		msg(llm.RoleUser, "tail2"),
		msg(llm.RoleAssistant, "tail3"),
	}
	got := c.Snip(msgs)
	if !strings.Contains(got[2].Content[0].ToolResult, "[snipped:") {
		t.Errorf("with empty ProtectedTools, memory_query must be snipped; got %q",
			truncate(got[2].Content[0].ToolResult, 60))
	}
}

// --- #5: [REDACTED] secret scrubbing ---------------------------------------

func TestRedactSecrets_StripsKnownKeyShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"openai-style", "auth uses sk-proj-AbCdEfGh1234567890mnop", "auth uses [REDACTED]"},
		{"github-pat", "pushed via github_pat_aaaabbbbccccddddeeeeffff1111", "pushed via [REDACTED]"},
		{"aws-akia", "AWS key AKIAIOSFODNN7EXAMPLE in env", "AWS key [REDACTED] in env"},
		{"named-kv", `api_key: "abcdef0123456789ABCD"`, `api_key: [REDACTED]`},
		{"jwt-token", "header: eyJhbGciOi.eyJzdWIiOiJhbGljZSJ9.SflKxw", "header: [REDACTED]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.in)
			if got != tc.want {
				t.Errorf("redactSecrets(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactSecrets_LeavesInnocuousTextAlone(t *testing.T) {
	in := "user wrote: the function called foo() and returned bar — see file.go:42"
	if got := redactSecrets(in); got != in {
		t.Errorf("redactSecrets mutated innocuous text: %q → %q", in, got)
	}
}

func TestRedactSecrets_IdempotentReapplication(t *testing.T) {
	once := redactSecrets("sk-pHasdf01234567890abcd")
	twice := redactSecrets(once)
	if once != twice {
		t.Errorf("redactSecrets not idempotent: once=%q twice=%q", once, twice)
	}
}

func TestSummarize_RedactsBeforeSendingToLLM(t *testing.T) {
	// Secret must land in the MIDDLE region (msgs[ProtectFirst:cut])
	// — Compact only summarizes the middle, so a secret in msgs[0]
	// (keepFirst) or the last 3 (keepLast) wouldn't reach summarize.
	p := &streamingProvider{}
	c := newCompactorForV2(p)
	c.RedactSecrets = true

	msgs := []llm.Message{
		msg(llm.RoleUser, "system seed"),
		msg(llm.RoleAssistant, "ack"),
		msg(llm.RoleUser, "set sk-secret_DO_NOT_LEAK_1234567890abcd in env"),
		msg(llm.RoleAssistant, "done"),
		msg(llm.RoleUser, "follow-up"),
		msg(llm.RoleAssistant, "more ack"),
		msg(llm.RoleUser, "tail1"),
		msg(llm.RoleAssistant, "tail2"),
		msg(llm.RoleUser, "tail3"),
	}
	if _, err := c.Compact(context.Background(), msgs); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if strings.Contains(p.payload, "DO_NOT_LEAK") {
		t.Errorf("summarize payload leaked secret: %q", truncate(p.payload, 300))
	}
	if !strings.Contains(p.payload, "[REDACTED]") {
		t.Errorf("summarize payload missing [REDACTED] marker: %q", truncate(p.payload, 300))
	}
}

func TestSummarize_DisableRedactsKeepsSecretsVisible(t *testing.T) {
	// Opt-out path — used by tests that want to assert prompt
	// contents verbatim without the regex pass interfering.
	p := &streamingProvider{}
	c := newCompactorForV2(p)
	c.RedactSecrets = false

	msgs := []llm.Message{
		msg(llm.RoleUser, "system seed"),
		msg(llm.RoleAssistant, "ack"),
		msg(llm.RoleUser, "sk-leakable_visible_123456789abcd is here"),
		msg(llm.RoleAssistant, "ok"),
		msg(llm.RoleUser, "tail1"),
		msg(llm.RoleAssistant, "tail2"),
		msg(llm.RoleUser, "tail3"),
	}
	if _, err := c.Compact(context.Background(), msgs); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !strings.Contains(p.payload, "sk-leakable_visible") {
		t.Errorf("RedactSecrets=false must pass secret through; payload=%q",
			truncate(p.payload, 200))
	}
}

// --- #6: token estimation regimes ------------------------------------------

func TestEstimateStringTokens_EnglishChars4(t *testing.T) {
	got := estimateStringTokens("hello world this is plain english text")
	want := len("hello world this is plain english text") / 4
	if got != want {
		t.Errorf("english chars/4: got %d want %d", got, want)
	}
}

func TestEstimateStringTokens_CJKChars1(t *testing.T) {
	// 12 Han characters → 12 tokens (1 token per glyph).
	got := estimateStringTokens("你好世界这是中文测试用例")
	if got < 10 || got > 14 {
		t.Errorf("CJK chars/1 should give ~12 tokens for 12 chars; got %d", got)
	}
}

func TestEstimateStringTokens_JsonHeavyDenserThanEnglish(t *testing.T) {
	json := `{"name":"x","value":"y","nested":{"k":"v","arr":[1,2,3]}}`
	english := strings.Repeat("a", len(json))
	gj := estimateStringTokens(json)
	ge := estimateStringTokens(english)
	if gj <= ge {
		t.Errorf("JSON should estimate denser than plain english of same length; json=%d english=%d", gj, ge)
	}
}

func TestEstimateStringTokens_Empty(t *testing.T) {
	if got := estimateStringTokens(""); got != 0 {
		t.Errorf("empty string should be 0; got %d", got)
	}
}

func TestEstimateTokens_ToolResultCountedViaContentAwareRatio(t *testing.T) {
	// Tool result body is mostly CJK → must be billed at CJK rates,
	// not chars/4. Pins the connection between estimateTokens and
	// estimateStringTokens.
	cjk := strings.Repeat("中", 200) // 200 Han runes
	msgs := []llm.Message{
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				{Type: "tool_result", ToolUseID: "u1", ToolResult: cjk},
			},
		},
	}
	got := estimateTokens(msgs)
	// 1 token per Han ≈ 200 + (envelope 4 + 8 + tiny). Loose lower
	// bound to keep the test stable against envelope tweaks.
	if got < 180 {
		t.Errorf("CJK tool_result undercounted; got %d (expected ≥180)", got)
	}
}

// --- #8: summarize retry + non-stream fallback -----------------------------

func TestSummarize_RetriesOnTransientErrorAndSucceeds(t *testing.T) {
	flaky := &flakyProvider{failsBeforeOK: 1, resp: "OK_AFTER_RETRY"}
	c := newCompactorForV2(flaky)
	c.MaxSummaryRetries = 2

	out, err := c.Compact(context.Background(), longMiddle())
	if err != nil {
		t.Fatalf("Compact returned error despite retry budget: %v", err)
	}
	if flaky.calls != 2 {
		t.Errorf("expected 2 Stream calls (1 fail + 1 success); got %d", flaky.calls)
	}
	if !containsBoundary(out, "OK_AFTER_RETRY") {
		t.Errorf("compact output missing retry summary; calls=%d", flaky.calls)
	}
}

func TestSummarize_FallsBackToCompleteWhenAllStreamsFail(t *testing.T) {
	prov := &alwaysFailStreamProvider{completeText: "COMPLETE_RESCUE"}
	c := newCompactorForV2(prov)
	c.MaxSummaryRetries = 2

	out, err := c.Compact(context.Background(), longMiddle())
	if err != nil {
		t.Fatalf("Compact must succeed via Complete fallback; got %v", err)
	}
	// Stream attempted (1 + 2 retries) = 3 times, Complete once.
	if prov.streamCalls != 3 {
		t.Errorf("expected 3 Stream calls; got %d", prov.streamCalls)
	}
	if prov.completeCalls != 1 {
		t.Errorf("expected exactly 1 Complete call; got %d", prov.completeCalls)
	}
	if !containsBoundary(out, "COMPLETE_RESCUE") {
		t.Errorf("fallback summary missing from boundary message")
	}
}

func TestSummarize_RespectsCtxCancelDuringBackoff(t *testing.T) {
	flaky := &flakyProvider{failsBeforeOK: 10} // fail forever
	c := newCompactorForV2(flaky)
	c.MaxSummaryRetries = 5

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE Compact runs

	_, err := c.Compact(ctx, longMiddle())
	if err == nil {
		t.Fatalf("expected error from cancelled ctx")
	}
}

// --- test utility ----------------------------------------------------------

func containsBoundary(messages []llm.Message, body string) bool {
	for _, m := range messages {
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, body) {
				return true
			}
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}

// silence unused-import alarm when io drops out of one rewrite cycle.
var _ = io.EOF
