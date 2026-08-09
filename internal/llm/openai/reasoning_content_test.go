package openai

import (
	"io"
	"strings"
	"testing"
)

func TestOpenAIStream_ReasoningContentIsEmittedBeforeTextAndTool(t *testing.T) {
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"先检查目录","content":"我来处理","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"/tmp/a\"}"}}]},"index":0}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls","index":0}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := makeStream(payload)
	defer s.Close()

	want := []struct {
		typ  string
		text string
	}{
		{typ: "thinking_delta", text: "先检查目录"},
		{typ: "text_delta", text: "我来处理"},
		{typ: "tool_use_start"},
		{typ: "tool_input_delta"},
		{typ: "tool_use_stop"},
		{typ: "message_delta"},
		{typ: "message_stop"},
	}
	for i, expected := range want {
		ev, err := s.Recv()
		if i == len(want)-1 {
			if err != io.EOF {
				t.Fatalf("event %d: want EOF, got %v", i, err)
			}
		} else if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if ev.Type != expected.typ || ev.TextDelta != expected.text {
			t.Fatalf("event %d = (%q, %q), want (%q, %q)", i, ev.Type, ev.TextDelta, expected.typ, expected.text)
		}
	}
}

func TestOpenAIStream_ReasoningAliasAndPriority(t *testing.T) {
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"canonical","reasoning":"duplicate"},"index":0}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning":" alias-only"},"index":0}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := makeStream(payload)
	defer s.Close()

	for i, want := range []string{"canonical", " alias-only"} {
		ev, err := s.Recv()
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if ev.Type != "thinking_delta" || ev.TextDelta != want {
			t.Fatalf("event %d = %+v, want thinking_delta %q", i, ev, want)
		}
	}
}

func TestFromOpenAIChoice_PreservesReasoningBeforeVisibleContent(t *testing.T) {
	reasoning := "需要先读取配置"
	choice := oaiChoice{FinishReason: "stop"}
	choice.Message.ReasoningContent = &reasoning
	choice.Message.Content = "最终回答"

	got := fromOpenAIChoice(choice, oaiUsage{})
	if len(got.Content) != 2 {
		t.Fatalf("content = %+v", got.Content)
	}
	if got.Content[0].Type != "thinking" || got.Content[0].Text != reasoning {
		t.Fatalf("first block = %+v, want thinking", got.Content[0])
	}
	if got.Content[1].Type != "text" || got.Content[1].Text != "最终回答" {
		t.Fatalf("second block = %+v, want text", got.Content[1])
	}
}

func TestFromOpenAIChoice_ReasoningAlias(t *testing.T) {
	reasoning := "vLLM alias"
	choice := oaiChoice{}
	choice.Message.Reasoning = &reasoning

	got := fromOpenAIChoice(choice, oaiUsage{})
	if len(got.Content) != 1 || got.Content[0].Type != "thinking" || got.Content[0].Text != reasoning {
		t.Fatalf("content = %+v", got.Content)
	}
}

func TestOpenAIStream_NullAndEmptyReasoningDoNotCreateRows(t *testing.T) {
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":null,"reasoning":"","content":"answer"},"index":0}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := makeStream(payload)
	defer s.Close()

	ev, err := s.Recv()
	if err != nil || ev.Type != "text_delta" || ev.TextDelta != "answer" {
		t.Fatalf("first event = %+v, err=%v", ev, err)
	}
}

func TestToOpenAI_ReplaysCapturedThinkingAsReasoningContent(t *testing.T) {
	req := Request{Messages: []Message{{
		Role: RoleAssistant,
		Content: []ContentBlock{
			{Type: "thinking", Text: "inspect config first"},
			{Type: "tool_use", ToolUseID: "call_1", ToolName: "Read", ToolInput: map[string]any{"path": "/tmp/config"}},
		},
	}}}

	body := toOpenAI(req, "deepseek-v4-flash", 4096)
	if len(body.Messages) != 1 {
		t.Fatalf("messages = %+v", body.Messages)
	}
	got := body.Messages[0]
	if got.ReasoningContent == nil || *got.ReasoningContent != "inspect config first" {
		t.Fatalf("reasoning_content = %#v", got.ReasoningContent)
	}
	if got.Reasoning != nil {
		t.Fatalf("outgoing history must use canonical reasoning_content, got reasoning=%q", *got.Reasoning)
	}
}
