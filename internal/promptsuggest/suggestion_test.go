package promptsuggest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func textMessage(role provider.Role, text string) provider.Message {
	return provider.Message{Role: role, Content: []provider.ContentBlock{{Type: "text", Text: text}}}
}

func TestLatestExchangeUsesOnlyCompletedVisibleConversation(t *testing.T) {
	history := []provider.Message{
		textMessage(provider.RoleUser, "修复输入框"),
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "tool_use", ToolUseID: "call-1", ToolName: "Read"}}},
		{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "tool_result", ToolUseID: "call-1", ToolResult: "private tool output"}}},
		{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "thinking", Text: "private reasoning"}, {Type: "text", Text: "输入框已修复。"}}},
	}
	before, _ := json.Marshal(history)
	got, ok := LatestExchange(history)
	if !ok || got.User != "修复输入框" || got.Assistant != "输入框已修复。" {
		t.Fatalf("exchange = %#v, %v", got, ok)
	}
	after, _ := json.Marshal(history)
	if string(before) != string(after) {
		t.Fatal("extracting a suggestion changed conversation history")
	}
}

func TestLatestExchangeRejectsIncompleteOrEmptyTurns(t *testing.T) {
	for _, history := range [][]provider.Message{
		nil,
		{textMessage(provider.RoleUser, "hello")},
		{textMessage(provider.RoleAssistant, "hello")},
		{textMessage(provider.RoleUser, "hello"), textMessage(provider.RoleAssistant, "")},
		{textMessage(provider.RoleUser, "old"), textMessage(provider.RoleAssistant, "done"), textMessage(provider.RoleUser, "new")},
		{textMessage(provider.RoleUser, "task"), {Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: "text", Text: "Working"}, {Type: "tool_use", ToolName: "Bash"}}}},
	} {
		if got, ok := LatestExchange(history); ok {
			t.Fatalf("incomplete conversation produced %#v", got)
		}
	}
}

type suggestionProvider struct {
	complete func(context.Context, provider.Request) (*provider.Response, error)
}

func (*suggestionProvider) Name() string          { return "fixture" }
func (*suggestionProvider) ModelID() string       { return "configured-model" }
func (*suggestionProvider) MaxContextTokens() int { return 128000 }
func (p *suggestionProvider) Complete(ctx context.Context, req provider.Request) (*provider.Response, error) {
	return p.complete(ctx, req)
}
func (*suggestionProvider) Stream(context.Context, provider.Request) (provider.StreamReader, error) {
	return nil, errors.New("suggestions must not start the main streaming loop")
}

func TestGenerateUsesBoundedToolFreeRequestAndCurrentModel(t *testing.T) {
	p := &suggestionProvider{complete: func(ctx context.Context, req provider.Request) (*provider.Response, error) {
		if req.Model != "selected-model" || len(req.Tools) != 0 || req.Stream || req.SessionID != "" {
			t.Fatalf("unexpected model/tool/session routing: %#v", req)
		}
		if req.MaxTokens != 256 || len(req.Messages) != 1 {
			t.Fatalf("unbounded suggestion request: %#v", req)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 8*time.Second || time.Until(deadline) <= 0 {
			t.Fatal("suggestion request has no bounded timeout")
		}
		payload := req.Messages[0].Content[0].Text
		if !utf8.ValidString(payload) || strings.Count(payload, "界") > 4096 || strings.Count(payload, "文") > 4096 {
			t.Fatal("context is unbounded or splits Unicode")
		}
		return &provider.Response{StopReason: "end_turn", Content: []provider.ContentBlock{{Type: "text", Text: "给这个改动补测试"}}}, nil
	}}
	got, err := Generate(context.Background(), p, "selected-model", Exchange{User: strings.Repeat("界", 6000), Assistant: strings.Repeat("文", 6000)})
	if err != nil || got != "给这个改动补测试" {
		t.Fatalf("suggestion = %q, %v", got, err)
	}
}

func TestGenerateRejectsUnsafeOrIncompleteOutput(t *testing.T) {
	for _, tc := range []struct {
		name, text, stop, want string
		tool                   bool
	}{
		{"none", "NONE", "end_turn", "", false},
		{"empty", "  ", "end_turn", "", false},
		{"control text", "\x1b[31m继续\x1b[0m\a\n补充测试", "end_turn", "继续 补充测试", false},
		{"too long", strings.Repeat("界", 161), "end_turn", "", false},
		{"code fence", "```sh\necho wrong\n```", "end_turn", "", false},
		{"truncated", "继续", "max_tokens", "", false},
		{"tool call", "继续", "tool_use", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &suggestionProvider{complete: func(context.Context, provider.Request) (*provider.Response, error) {
				content := []provider.ContentBlock{{Type: "text", Text: tc.text}}
				if tc.tool {
					content = append(content, provider.ContentBlock{Type: "tool_use", ToolName: "Bash"})
				}
				return &provider.Response{Content: content, StopReason: tc.stop}, nil
			}}
			got, err := Generate(context.Background(), p, "selected-model", Exchange{User: "修复问题", Assistant: "已修复"})
			if err != nil || got != tc.want {
				t.Fatalf("suggestion = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestGenerateDoesNotCallProviderWithoutEligibleContext(t *testing.T) {
	p := &suggestionProvider{complete: func(context.Context, provider.Request) (*provider.Response, error) {
		t.Fatal("provider was called for an ineligible suggestion")
		return nil, nil
	}}
	for _, exchange := range []Exchange{{}, {User: "hello"}, {Assistant: "hello"}} {
		if text, err := Generate(context.Background(), p, "model", exchange); text != "" || err != nil {
			t.Fatalf("ineligible result = %q, %v", text, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Generate(ctx, p, "model", Exchange{User: "task", Assistant: "done"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled generation = %v", err)
	}
}

func TestGeneratePropagatesCancellationAndProviderFailure(t *testing.T) {
	p := &suggestionProvider{complete: func(ctx context.Context, _ provider.Request) (*provider.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := Generate(ctx, p, "model", Exchange{User: "task", Assistant: "done"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
	failure := errors.New("provider unavailable")
	p.complete = func(context.Context, provider.Request) (*provider.Response, error) { return nil, failure }
	if _, err := Generate(context.Background(), p, "model", Exchange{User: "task", Assistant: "done"}); !errors.Is(err, failure) {
		t.Fatalf("provider error = %v", err)
	}
}
