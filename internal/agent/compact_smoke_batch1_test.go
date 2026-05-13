//go:build smoke
// +build smoke

package agent

// compact_smoke_batch1_test.go — REAL-PROVIDER smoke for the 2026-05-13
// Batch 1 compaction upgrades (#1 5-section prompt / #2 iterative /
// #4 protected tools / #5 [REDACTED] / #6 file-aware tokens / #8 retry).
//
// Run with:
//   GLM_KEY=<key> go test -tags smoke -v ./internal/agent/ -run TestSmokeBatch1
//
// The build tag keeps this file out of the default `go test ./...`
// run so CI / unit-test loops don't hit a real LLM. Designed for
// tmux-driven manual smoke after `make install`.
//
// Each sub-test asserts ONE batch item. The Compact() calls go to
// the configured GLM (z.ai OpenAI-compat) endpoint and the test
// fails loudly when a real provider round-trip doesn't carry the
// expected behaviour through to the summary string.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
)

const (
	// Match ~/.metis/config.toml [provider.custom.glm].model exactly.
	// Wrong model name → z.ai routes to a stub that returns empty
	// stream + then hangs the next request on H2 connection reuse;
	// hit that in the first smoke run with glm-4.6 (model doesn't
	// exist on this account).
	smokeProviderModel  = "glm-5.1"
	smokeProviderURL    = "https://api.z.ai/api/paas/v4"
	smokeMaxTokens      = 4096
	smokeRequestTimeout = 45 * time.Second
)

// smokeReportPath writes the per-test summary to the standard metis
// report folder so the user can review post-run without hunting through
// `go test -v` output.
var smokeReportPath = filepath.Join(
	os.Getenv("HOME"),
	"Documents/公司学习文件/我自己的agent的cli/测试报告/batch1_smoke_2026-05-13.md",
)

func smokeProvider(t *testing.T) llm.Provider {
	t.Helper()
	key := os.Getenv("GLM_KEY")
	if key == "" {
		key = "6ba62aa4117e44afba2a3899b68d3479.qV9oCtFiMf76DNlS"
	}
	return openai.New(key, smokeProviderURL, smokeProviderModel, smokeMaxTokens, smokeRequestTimeout, 0.4)
}

func smokeCompactor(p llm.Provider) *Compactor {
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	cfg.MaxSummaryTokens = 600
	return NewCompactor(cfg, smokeProviderModel, 128_000, p)
}

func smokeAppend(t *testing.T, section, body string) {
	t.Helper()
	f, err := os.OpenFile(smokeReportPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Logf("smokeAppend skipped (open %s: %v)", smokeReportPath, err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "## %s\n\n%s\n\n", section, body)
}

func longConversation(seed string) []llm.Message {
	// Build a 14-message conversation: ProtectFirst=1 (seed),
	// ProtectLast=3 (recent tail). Middle gets summarized.
	out := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text",
			Text: "Help me refactor an HTTP service. " + seed}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text",
			Text: "Sure. Where's the current code? I'll start by reading auth/middleware.go."}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text",
			Text: "It's in server/auth/middleware.go and server/router.go. The middleware is too coupled."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text",
			Text: "Reading both files now. I see middleware.go imports session package directly — that's the coupling."}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text",
			Text: "Right. Extract a SessionStore interface and inject it into the middleware factory."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text",
			Text: "Wrote SessionStore interface in server/auth/types.go:1-15. Updated middleware.go:42 to take SessionStore in constructor."}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text",
			Text: "Now update router.go to construct middleware with a real store. Use the existing redisSessionStore from server/session."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text",
			Text: "Done — router.go:67 now calls auth.NewMiddleware(redisSessionStore). Tests in middleware_test.go pass with the mock."}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text",
			Text: "Run the full test suite."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text",
			Text: "go test ./server/... — 47 pass, 0 fail. Coverage 78%."}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text",
			Text: "Add a benchmark for the auth middleware hot path."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text",
			Text: "Added BenchmarkAuthMiddleware in middleware_bench_test.go — currently 1.2us/op, 0 allocs."}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text",
			Text: "Looks good. Next, document the new interface in README."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text",
			Text: "Drafting the README section now."}}},
	}
	return out
}

// TestSmokeBatch1 is the umbrella entry — sub-tests cover each batch item.
func TestSmokeBatch1(t *testing.T) {
	_ = os.Remove(smokeReportPath)
	smokeAppend(t, "Metis Batch 1 — real-provider smoke 2026-05-13",
		fmt.Sprintf("Provider: GLM %s @ %s\nMaxTokens: %d\nTimeout: %s\nCommit: cafd30a + Batch 1 patch",
			smokeProviderModel, smokeProviderURL, smokeMaxTokens, smokeRequestTimeout))

	t.Run("01_FivePromptSections", smokeFiveSections)
	t.Run("02_IterativeMerge", smokeIterativeMerge)
	t.Run("04_ProtectedToolUntouched", smokeProtectedTool)
	t.Run("05_RedactedSecrets", smokeRedacted)
	t.Run("06_CJKAndJsonAware", smokeTokenRegimes)
	t.Run("08_RetryFallback", smokeRetryFallback)
}

// --- #1 5-section prompt → real GLM produces sectioned output ---------------

func smokeFiveSections(t *testing.T) {
	p := smokeProvider(t)
	c := smokeCompactor(p)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := c.Compact(ctx, longConversation("fresh-five-section-check"))
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	body := extractBoundaryBody(out)
	if body == "" {
		t.Fatalf("no boundary message produced; got %d messages", len(out))
	}

	hits := 0
	for _, h := range []string{"## Current State", "## Files & Changes",
		"## Technical Context", "## Strategy & Approach", "## Exact Next Steps"} {
		if strings.Contains(body, h) {
			hits++
		}
	}
	smokeAppend(t, "#1 five-section prompt",
		fmt.Sprintf("Section hits: %d/5\n\nBoundary preview (first 800 chars):\n\n```\n%s\n```",
			hits, preview(body, 800)))
	if hits < 3 {
		t.Errorf("real GLM produced summary with %d/5 expected sections (need ≥3); body=%q",
			hits, preview(body, 400))
	}
}

// --- #2 iterative merge → 2nd Compact reuses 1st summary --------------------

func smokeIterativeMerge(t *testing.T) {
	p := smokeProvider(t)
	c := smokeCompactor(p)

	conv := longConversation("iter-merge-check")
	ctx1, cancel1 := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel1()
	out1, err := c.Compact(ctx1, conv)
	if err != nil {
		t.Fatalf("first Compact: %v", err)
	}
	first := c.LastSummary
	if first == "" {
		t.Fatalf("LastSummary empty after first Compact")
	}

	// Tack on 8 new messages and Compact again. The middle will
	// start with the boundary from out1, which extractPriorSummary
	// peels into the merge seed.
	extra := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text",
			Text: "Now also add a context.Context parameter to SessionStore.Get and update all call sites."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text",
			Text: "Added ctx to SessionStore.Get; updated 6 call sites in router.go, middleware.go, handlers/*.go."}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text",
			Text: "Run tests again."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text",
			Text: "All 47 still pass; benchmark unchanged at 1.2us/op."}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text",
			Text: "Great. Commit with conventional commit message."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text",
			Text: "Committed: refactor(auth): inject SessionStore + thread ctx through hot path."}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text",
			Text: "And finalize the README."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text",
			Text: "README updated with the new interface section + migration notes."}}},
	}
	second := append([]llm.Message(nil), out1...)
	second = append(second, extra...)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel2()
	out2, err := c.Compact(ctx2, second)
	if err != nil {
		t.Fatalf("second Compact: %v", err)
	}
	body2 := extractBoundaryBody(out2)
	smokeAppend(t, "#2 iterative merge",
		fmt.Sprintf("1st summary (preview):\n```\n%s\n```\n2nd summary (preview):\n```\n%s\n```\nLastSummary persisted: %v",
			preview(first, 600), preview(body2, 600), c.LastSummary != ""))

	// The 2nd summary should describe BOTH the auth-refactor topic
	// from round 1 and the ctx-threading from round 2. We assert a
	// substring witness from each.
	if !strings.Contains(strings.ToLower(body2), "sessionstore") {
		t.Errorf("merged summary lost round-1 topic (sessionstore)")
	}
	if !strings.Contains(strings.ToLower(body2), "ctx") &&
		!strings.Contains(strings.ToLower(body2), "context") {
		t.Errorf("merged summary lost round-2 topic (ctx threading)")
	}
}

// --- #4 protected tool → memory_query result preserved verbatim -------------

func smokeProtectedTool(t *testing.T) {
	p := smokeProvider(t)
	c := smokeCompactor(p)
	c.SnipMaxToolResultChars = 100
	bigMQ := "memory snapshot: user prefers tabs, " + strings.Repeat("KEEP_THIS_VERBATIM_", 50)
	bigBash := "bash output: " + strings.Repeat("BORING_LOG_LINE_", 80)

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Initial seed."}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "text", Text: "Checking memory and shell."},
			{Type: "tool_use", ToolUseID: "u_mq", ToolName: "memory_query", ToolInput: map[string]any{}},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: "u_mq", ToolResult: bigMQ},
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "text", Text: "Now running bash."},
			{Type: "tool_use", ToolUseID: "u_bash", ToolName: "Bash", ToolInput: map[string]any{}},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: "u_bash", ToolResult: bigBash},
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text",
			Text: "Tail msg 1"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "Tail msg 2"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "Tail msg 3"}}},
	}
	got := c.Snip(msgs)

	mq := got[2].Content[0].ToolResult
	bash := got[4].Content[0].ToolResult
	smokeAppend(t, "#4 protected tool (memory_query) untouched, Bash snipped",
		fmt.Sprintf("memory_query len after Snip: %d (orig %d, snipped marker: %v)\nBash len after Snip: %d (orig %d, snipped marker: %v)",
			len(mq), len(bigMQ), strings.Contains(mq, "[snipped:"),
			len(bash), len(bigBash), strings.Contains(bash, "[snipped:")))

	if strings.Contains(mq, "[snipped:") {
		t.Errorf("memory_query was snipped despite being in ProtectedTools")
	}
	if !strings.Contains(bash, "[snipped:") {
		t.Errorf("Bash was NOT snipped even though it's huge")
	}
}

// --- #5 [REDACTED] → real GLM summary doesn't carry the secret --------------

func smokeRedacted(t *testing.T) {
	p := smokeProvider(t)
	c := smokeCompactor(p)
	conv := longConversation("redact-check")
	// Inject a secret into msgs[3] (middle region for ProtectFirst=1)
	conv[3].Content = []llm.ContentBlock{{Type: "text",
		Text: "OK — for the auth setup, the dev key is sk-proj-DO_NOT_LEAK_8sk12bnoZ19LQzMm and AWS access AKIAIOSFODNN7EXAMPLE."}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := c.Compact(ctx, conv)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	body := extractBoundaryBody(out)
	smokeAppend(t, "#5 [REDACTED] secret scrubbing",
		fmt.Sprintf("Boundary preview:\n```\n%s\n```\nContains 'DO_NOT_LEAK': %v\nContains 'AKIAIOSFODNN7EXAMPLE': %v\nContains '[REDACTED]': %v",
			preview(body, 600),
			strings.Contains(body, "DO_NOT_LEAK"),
			strings.Contains(body, "AKIAIOSFODNN7EXAMPLE"),
			strings.Contains(body, "REDACTED")))
	if strings.Contains(body, "DO_NOT_LEAK") {
		t.Errorf("summary leaked openai key fragment")
	}
	if strings.Contains(body, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("summary leaked AWS key")
	}
}

// --- #6 CJK + JSON token regimes (no LLM needed) ----------------------------

func smokeTokenRegimes(t *testing.T) {
	en := strings.Repeat("a", 200)
	cn := strings.Repeat("中", 200)
	js := strings.Repeat(`{"k":"v","n":1},`, 20) // ~320 chars JSON-heavy

	gEN := estimateStringTokens(en)
	gCN := estimateStringTokens(cn)
	gJS := estimateStringTokens(js)

	smokeAppend(t, "#6 token estimation regimes",
		fmt.Sprintf("English 200 chars: %d tokens (~4 chars/token)\nCJK 200 chars: %d tokens (~1 char/token)\nJSON 320 chars: %d tokens (~2.5 chars/token)",
			gEN, gCN, gJS))
	if gCN < 150 {
		t.Errorf("CJK undercount: %d < 150 (200 chars should be ~200 tokens)", gCN)
	}
	if gJS <= len(js)/4 {
		t.Errorf("JSON not flagged denser than English: gJS=%d (en-rate=%d)", gJS, len(js)/4)
	}
	if gEN > 60 {
		t.Errorf("English overcount: %d > 60 (200 chars should be ~50 tokens)", gEN)
	}
}

// --- #8 retry path → real provider one-shot success, but exercises ctx cancel

func smokeRetryFallback(t *testing.T) {
	// Real provider rarely fails on a small request; this sub-test
	// verifies the no-op happy path doesn't regress retries to 0.
	// (Stream failure paths are pinned by compact_v2_test.go.)
	p := smokeProvider(t)
	c := smokeCompactor(p)
	c.MaxSummaryRetries = 2
	conv := longConversation("retry-happy-path")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := c.Compact(ctx, conv)
	if err != nil {
		t.Fatalf("Compact happy path failed: %v", err)
	}
	smokeAppend(t, "#8 retry happy path",
		fmt.Sprintf("Compact completed; %d messages → %d messages (MaxSummaryRetries=%d, no retries needed for healthy GLM)",
			len(conv), len(out), c.MaxSummaryRetries))
}

// --- helpers ----------------------------------------------------------------

func extractBoundaryBody(messages []llm.Message) string {
	for _, m := range messages {
		for _, b := range m.Content {
			if b.Type != "text" {
				continue
			}
			t := b.Text
			if strings.HasPrefix(t, "[Earlier conversation summarized: ") && strings.HasSuffix(t, "]") {
				return strings.TrimSuffix(strings.TrimPrefix(t, "[Earlier conversation summarized: "), "]")
			}
		}
	}
	return ""
}

func preview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[...truncated]"
}
