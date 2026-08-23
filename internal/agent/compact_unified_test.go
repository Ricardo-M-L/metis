package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestCompactUnified_DropsAncientGreeting pins the most important retention
// rule of the unified compaction pipeline: the first chat message is not a
// system prompt and must not be permanently pinned. Current authoritative
// system state is rebuilt by Loop.buildRequest; compacted history should be a
// checkpoint plus a recent verbatim tail.
func TestCompactUnified_DropsAncientGreeting(t *testing.T) {
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	c.ProtectLast = 3

	messages := []llm.Message{msg(llm.RoleUser, "ANCIENT_GREETING_MUST_GO")}
	for i := 0; i < 8; i++ {
		messages = append(messages,
			msg(llm.RoleAssistant, "old answer "+istr(i)),
			msg(llm.RoleUser, "old follow-up "+istr(i)),
		)
	}
	messages = append(messages,
		msg(llm.RoleUser, "SECOND_LATEST_REAL_USER_REQUEST"),
		msg(llm.RoleAssistant, "working"),
		msg(llm.RoleUser, "LATEST_REAL_USER_REQUEST"),
		msg(llm.RoleAssistant, "latest answer"),
	)

	out, err := c.Compact(context.Background(), messages)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	for _, m := range out {
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, "ANCIENT_GREETING_MUST_GO") {
				t.Fatalf("ancient greeting remained verbatim after compaction: %#v", out)
			}
		}
	}
}

func TestCompactUnified_RetainsLatestTwoRealUserRequests(t *testing.T) {
	p := &fakeSummarizer{}
	c := newCompactorFor(p)
	c.ProtectLast = 3 // the legacy fixed tail would lose the second request

	messages := []llm.Message{msg(llm.RoleUser, "old greeting")}
	for i := 0; i < 5; i++ {
		messages = append(messages,
			msg(llm.RoleAssistant, "old answer "+istr(i)),
			msg(llm.RoleUser, "old prompt "+istr(i)),
		)
	}
	messages = append(messages, msg(llm.RoleUser, "SECOND_LATEST_REAL_USER_REQUEST"))
	for i := 0; i < 4; i++ {
		id := "recent-tool-" + istr(i)
		messages = append(messages,
			toolUseMsg(id, "Read"),
			toolResultMsg(id, "recent evidence "+istr(i)),
		)
	}
	messages = append(messages,
		msg(llm.RoleUser, "LATEST_REAL_USER_REQUEST"),
		msg(llm.RoleAssistant, "answer in progress"),
	)

	out, err := c.Compact(context.Background(), messages)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	for _, want := range []string{"SECOND_LATEST_REAL_USER_REQUEST", "LATEST_REAL_USER_REQUEST"} {
		if !historyHasText(out, want) {
			t.Fatalf("latest user request %q was not retained verbatim: %#v", want, out)
		}
	}
	assertBalancedToolPairs(t, out)
}

func historyHasText(messages []llm.Message, needle string) bool {
	for _, m := range messages {
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, needle) {
				return true
			}
		}
	}
	return false
}

func assertBalancedToolPairs(t *testing.T, messages []llm.Message) {
	t.Helper()
	uses := make(map[string]bool)
	results := make(map[string]bool)
	for _, m := range messages {
		for _, b := range m.Content {
			switch b.Type {
			case "tool_use":
				uses[b.ToolUseID] = true
			case "tool_result":
				results[b.ToolUseID] = true
			}
		}
	}
	for id := range uses {
		if !results[id] {
			t.Errorf("retained tool_use %q has no retained tool_result", id)
		}
	}
	for id := range results {
		if !uses[id] {
			t.Errorf("retained tool_result %q has no retained tool_use", id)
		}
	}
}

func TestSummaryPayloadPreservesToolIdentityInputAndResultTail(t *testing.T) {
	p := &streamingProvider{}
	c := newCompactorForV2(p)
	longResult := "HEAD_MARKER\n" + strings.Repeat("x", 1200) + "\nTAIL_MARKER"
	messages := []llm.Message{
		{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{{
				Type:      "tool_use",
				ToolUseID: "call-42",
				ToolName:  "Read",
				ToolInput: map[string]any{
					"file_path": "/tmp/important.go",
					"offset":    120,
				},
			}},
		},
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Type:       "tool_result",
				ToolUseID:  "call-42",
				ToolResult: longResult,
				IsError:    true,
			}},
		},
	}

	if _, err := c.summarize(context.Background(), messages, ""); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	for _, want := range []string{
		"call-42",
		"Read",
		"/tmp/important.go",
		"120",
		"HEAD_MARKER",
		"TAIL_MARKER",
		"is_error=true",
	} {
		if !strings.Contains(p.payload, want) {
			t.Errorf("summary payload lost %q:\n%s", want, p.payload)
		}
	}
}

type maxTokensSummaryProvider struct{}

func (*maxTokensSummaryProvider) Name() string          { return "max-tokens-summary" }
func (*maxTokensSummaryProvider) MaxContextTokens() int { return 100_000 }
func (*maxTokensSummaryProvider) ModelID() string       { return "" }
func (*maxTokensSummaryProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return &fakeStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "TRUNCATED_BUT_NONEMPTY"},
		{Type: "message_delta", StopReason: "max_tokens"},
		{Type: "message_stop"},
	}}, nil
}
func (*maxTokensSummaryProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return &llm.Response{
		Content:    []llm.ContentBlock{{Type: "text", Text: "TRUNCATED_FALLBACK"}},
		StopReason: "max_tokens",
	}, nil
}

func TestSummarizeRejectsMaxTokensStopReason(t *testing.T) {
	c := newCompactorForV2(&maxTokensSummaryProvider{})
	c.MaxSummaryRetries = 0
	_, err := c.summarize(context.Background(), []llm.Message{msg(llm.RoleUser, "important state")}, "")
	if err == nil {
		t.Fatal("summarize accepted a max_tokens-truncated checkpoint")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "max_tokens") {
		t.Fatalf("summarize error = %v, want max_tokens diagnostic", err)
	}
}

func TestDefaultCompactionConfigProtectsCurrentToolNames(t *testing.T) {
	cfg := DefaultCompactionConfig()
	for _, want := range []string{"Memory", "Skill"} {
		found := false
		for _, got := range cfg.ProtectedTools {
			if strings.EqualFold(got, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ProtectedTools missing current tool name %q: %v", want, cfg.ProtectedTools)
		}
	}
}

// Compile-time guards for the small provider used above. Keeping these here
// makes interface changes fail at the test fixture rather than deep in a run.
var _ llm.Provider = (*maxTokensSummaryProvider)(nil)
