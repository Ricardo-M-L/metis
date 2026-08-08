package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestConversationText_ClaudeStyleAndPrivacyBoundary(t *testing.T) {
	secret := "CUSTOM_API_KEY=abcdefghijklmnop123456"
	messages := []llm.Message{
		{
			Role: llm.RoleSystem,
			Content: []llm.ContentBlock{{
				Type: "text", Text: "SYSTEM_PROMPT_MUST_NOT_LEAK",
			}},
		},
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				{Type: "text", Text: "hello\nworld\n\x1b[31mred text\x1b[0m"},
				{Type: "image", MediaType: "image/png", Data: "BASE64_IMAGE_MUST_NOT_LEAK"},
			},
		},
		{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{
				{Type: "text", Text: "I will inspect it."},
				{
					Type:         "tool_use",
					ToolUseID:    "toolu_private_id",
					ToolName:     "Read",
					ProviderHint: map[string]string{"thought_signature": "OPAQUE_HINT_MUST_NOT_LEAK"},
					ToolInput: map[string]any{
						"path":          "/tmp/main.go",
						"api_key":       "raw-tool-secret",
						"provider_hint": "INTERNAL_INPUT_MUST_NOT_LEAK",
						"headers": map[string]string{
							"Authorization": "Bearer NESTED_HEADER_SECRET",
							"X-Normal":      "visible-header",
						},
						"steps": []string{"PASSWORD=NESTED_LIST_SECRET", "visible step"},
					},
				},
				{Type: "thinking", Text: "HIDDEN_REASONING_MUST_NOT_LEAK"},
			},
		},
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Type:       "tool_result",
				ToolUseID:  "toolu_private_id",
				ToolResult: "package main\n\n<system-reminder>INTERNAL_REMINDER_MUST_NOT_LEAK</system-reminder>\n" + secret,
				ToolResultBlocks: []llm.ContentBlock{{
					Type: "image", MediaType: "image/png", Data: "NESTED_BASE64_MUST_NOT_LEAK",
				}},
			}},
		},
		{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: "text", Text: "Done."}},
		},
	}

	got := conversationText(messages)
	for _, want := range []string{
		"❯ hello\n  world\n  red text",
		"❯ [image omitted from text export]",
		"⏺ I will inspect it.",
		`"headers":{"Authorization":"[REDACTED]","X-Normal":"visible-header"}`,
		`"steps":["PASSWORD=[REDACTED]","visible step"]`,
		`"api_key":"[REDACTED]"`,
		`"path":"/tmp/main.go"`,
		"⎿  Read: package main",
		"CUSTOM_API_KEY=[REDACTED]",
		"[image omitted from text export]",
		"⏺ Done.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("text export missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"SYSTEM_PROMPT_MUST_NOT_LEAK",
		"BASE64_IMAGE_MUST_NOT_LEAK",
		"NESTED_BASE64_MUST_NOT_LEAK",
		"OPAQUE_HINT_MUST_NOT_LEAK",
		"INTERNAL_INPUT_MUST_NOT_LEAK",
		"INTERNAL_REMINDER_MUST_NOT_LEAK",
		"HIDDEN_REASONING_MUST_NOT_LEAK",
		"NESTED_HEADER_SECRET",
		"NESTED_LIST_SECRET",
		"abcdefghijklmnop123456",
		"toolu_private_id",
		`"role"`,
		"\x1b[31m",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("text export leaked %q:\n%s", forbidden, got)
		}
	}
}

func TestConversationText_OmitsUnterminatedInternalReminder(t *testing.T) {
	got := conversationText([]llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "<system-reminder>private runtime instruction"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "visible prefix\n<system-reminder>private appended instruction"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "visible prompt"}}},
	})
	if strings.Contains(got, "private runtime instruction") || strings.Contains(got, "private appended instruction") {
		t.Fatalf("unterminated internal reminder leaked: %q", got)
	}
	if !strings.Contains(got, "⏺ visible prefix") {
		t.Fatalf("visible content before internal reminder was dropped: %q", got)
	}
	if !strings.Contains(got, "❯ visible prompt") {
		t.Fatalf("visible prompt missing after filtered reminder: %q", got)
	}
}

func TestExportFilename_UsesTimestampAndFirstVisiblePrompt(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 45, 49, 0, time.Local)
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "<system-reminder>internal</system-reminder>"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "This session is being continued from a previous conversation with more detail"}}},
	}
	got := exportFilename(messages, now)
	if !strings.HasPrefix(got, "2026-08-08-004549-this-session-is-being-continued-from-a-previous-") || !strings.HasSuffix(got, ".txt") {
		t.Fatalf("Claude-style filename = %q", got)
	}
	if strings.Contains(got, "conversation-conversation") {
		t.Fatalf("filename contains duplicated fallback: %q", got)
	}
}

func TestExportFilename_PreservesSafeUnicodePrompt(t *testing.T) {
	now := time.Date(2026, 8, 8, 1, 2, 3, 0, time.Local)
	messages := []llm.Message{{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "text", Text: "帮我检查导出格式"}},
	}}
	if got, want := exportFilename(messages, now), "2026-08-08-010203-帮我检查导出格式.txt"; got != want {
		t.Fatalf("unicode filename = %q, want %q", got, want)
	}
}

func TestExportConversationToFile_WritesPrivateReadableText(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	now := time.Date(2026, 8, 8, 1, 2, 3, 0, time.Local)
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello export"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "hello back"}}},
	}

	path, err := exportConversationToFile(messages, "", now)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(home, "exports", "2026-08-08-010203-hello-export.txt")
	if path != wantPath {
		t.Fatalf("path = %q, want %q", path, wantPath)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); !strings.Contains(got, "❯ hello export") || !strings.Contains(got, "⏺ hello back") {
		t.Fatalf("readable transcript missing messages: %q", got)
	}
	if strings.Contains(string(body), `"role"`) || strings.Contains(string(body), `"content"`) {
		t.Fatalf("export is still serialized JSON: %q", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("export mode = %o, want 600", got)
	}
}

func TestExportConversationToFile_ExplicitNameForcesTxt(t *testing.T) {
	dir := t.TempDir()
	requested := filepath.Join(dir, "share.jsonl")
	target := filepath.Join(dir, "share.txt")
	if err := os.WriteFile(target, []byte("old export"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := exportConversationToFile(nil, requested, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if want := target; path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("overwritten export mode = %o, want 600", got)
	}
	if _, err := os.Stat(requested); !os.IsNotExist(err) {
		t.Fatalf("legacy JSONL path should not be created; stat err=%v", err)
	}
}
