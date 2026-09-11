package tui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// TestRender_RedactedThinking_NeverShowsCipherText — the most
// important contract: a Message{Role:"redacted_thinking"} carries
// opaque cipher text in Content (needed for round-trip to the provider
// on the next turn), but rendering MUST NOT display that cipher text
// or a placeholder to the user.
func TestRender_RedactedThinking_NeverShowsCipherText(t *testing.T) {
	cipherText := "EuwBCkAGfXMSECRETKEY/abc+def=="
	msg := Message{
		Role:      "redacted_thinking",
		Content:   cipherText,
		Timestamp: time.Now(),
	}
	out := renderMessage(msg, 80, false)
	if strings.Contains(out, cipherText) {
		t.Fatalf("rendered output leaked the cipher text — this is a UI security regression: %q", out)
	}
	if strings.Contains(out, "SECRETKEY") {
		t.Fatalf("rendered output contains part of the cipher payload: %q", out)
	}
}

// Opaque replay data has no user-facing text, including in expanded mode.
func TestRender_RedactedThinking_HasNoPlaceholder(t *testing.T) {
	msg := Message{Role: "redacted_thinking", Content: "OPAQUE", Timestamp: time.Now()}
	for _, expand := range []bool{false, true} {
		if out := renderMessage(msg, 80, expand); out != "" {
			t.Errorf("encrypted reasoning must render nothing (expand=%v), got %q", expand, out)
		}
	}
}

// TestRender_RedactedThinking_ExpandFlagIgnored — the `expand` flag
// (ctrl+o state) is meaningless for redacted blocks since there's no
// plaintext to expand into. Rendered output must be identical whether
// expand=true or expand=false — neither renders a row.
func TestRender_RedactedThinking_ExpandFlagIgnored(t *testing.T) {
	msg := Message{Role: "redacted_thinking", Content: "CIPHER", Timestamp: time.Now()}
	collapsed := renderMessage(msg, 80, false)
	expanded := renderMessage(msg, 80, true)
	if collapsed != expanded {
		t.Errorf("ctrl+o (expand) must NOT change redacted_thinking rendering (no plaintext exists); diff:\ncollapsed=%q\nexpanded=%q", collapsed, expanded)
	}
}

// TestBuildChatItems_ThinkingDisplay_Hide_DropsBothRows — when the
// user runs `/thinking hide`, both normal thinking and redacted_thinking
// rows must be filtered out of the rendered transcript. The persisted
// Messages stay (so /thinking show brings public thinking back without
// a reload), only the visible item list shrinks.
func TestBuildChatItems_ThinkingDisplay_Hide_DropsBothRows(t *testing.T) {
	m := newTestModelForVisibility()
	m.thinkingDisplay = "hide"
	m.messages = []Message{
		{Role: "user", Content: "do X"},
		{Role: "thinking", Content: "let me think"},
		{Role: "redacted_thinking", Content: "CIPHER"},
		{Role: "assistant", Content: "done"},
	}
	items := m.buildChatItems()
	for _, it := range items {
		if mi, ok := it.(*messageItem); ok {
			if mi.msg.Role == "thinking" || mi.msg.Role == "redacted_thinking" {
				t.Errorf("/thinking hide leaked a %s row into the item list", mi.msg.Role)
			}
		}
	}
}

// TestBuildChatItems_ThinkingDisplay_Show_ForcesExpand — `/thinking show`
// must force normal thinking rows to render expanded. Redacted rows are
// unaffected (they have no expand state).
//
// P0-1 (2026-08-02): the global expandToolOutputs toggle was removed;
// thinking rows now honour /thinking show only. The comment about
// "even when expandToolOutputs=false" is preserved as historical
// context but the field no longer exists on the Model.
func TestBuildChatItems_ThinkingDisplay_Show_ForcesExpand(t *testing.T) {
	m := newTestModelForVisibility()
	m.thinkingDisplay = "show"
	m.messages = []Message{
		{Role: "thinking", Content: "multi-line\nreasoning trace"},
	}
	items := m.buildChatItems()
	for _, it := range items {
		mi, ok := it.(*messageItem)
		if !ok || mi.msg.Role != "thinking" {
			continue
		}
		if !mi.expand {
			t.Errorf("/thinking show must force thinking rows to render expanded")
		}
	}
}

// Auto keeps public thinking compact and omits encrypted replay data.
func TestBuildChatItems_ThinkingDisplay_Auto_KeepsPublicThinkingOnly(t *testing.T) {
	m := newTestModelForVisibility()
	m.thinkingDisplay = "auto"
	m.messages = []Message{
		{Role: "thinking", Content: "trace"},
		{Role: "redacted_thinking", Content: "CIPHER"},
	}
	items := m.buildChatItems()
	thinkingKept, redactedKept := false, false
	for _, it := range items {
		mi, ok := it.(*messageItem)
		if !ok {
			continue
		}
		if mi.msg.Role == "thinking" {
			thinkingKept = true
			if mi.expand {
				t.Errorf("/thinking auto must render thinking COLLAPSED (mi.expand=true is wrong)")
			}
		}
		if mi.msg.Role == "redacted_thinking" {
			redactedKept = true
		}
	}
	if !thinkingKept {
		t.Errorf("/thinking auto must keep thinking rows in the item list")
	}
	if redactedKept {
		t.Errorf("/thinking auto must omit redacted_thinking rows from the item list")
	}
}

func TestBuildChatItems_RedactedThinkingAlwaysHidden(t *testing.T) {
	for _, display := range []string{"auto", "show", "hide"} {
		for _, style := range []string{"", outputStyleFull, outputStyleStreamlined, outputStyleMinimal} {
			t.Run(display+"/"+style, func(t *testing.T) {
				m := newTestModelForVisibility()
				m.thinkingDisplay = display
				m.outputStyle = style
				m.messages = []Message{
					{Role: "user", Content: "question"},
					{Role: "thinking", Content: "public summary"},
					{Role: "redacted_thinking", Content: "CIPHER"},
					{Role: "assistant", Content: "answer"},
				}
				before := append([]Message(nil), m.messages...)
				var roles []string
				for _, item := range m.buildChatItems() {
					if mi, ok := item.(*messageItem); ok {
						roles = append(roles, mi.msg.Role)
					}
				}
				want := []string{"user", "assistant"}
				if display != "hide" && normalizeOutputStyle(style) == outputStyleFull {
					want = []string{"user", "thinking", "assistant"}
				}
				if !reflect.DeepEqual(roles, want) {
					t.Errorf("visible message rows = %v, want %v (no encrypted or blank row)", roles, want)
				}
				if !reflect.DeepEqual(m.messages, before) {
					t.Fatal("presentation filtering changed source messages")
				}
			})
		}
	}
}

// newTestModelForVisibility produces a Model with the minimum init
// needed for buildChatItems to walk without nil-deref. We bypass NewModel
// because that requires loop/registry/store/cfg — irrelevant for the
// pure-rendering tests above. The welcome banner needs a non-nil gate
// (renderWelcomeBannerCard reads gate.Mode()).
func newTestModelForVisibility() *Model {
	return &Model{
		width: 80, height: 24,
		gate: permission.New(permission.ModeBypass),
	}
}
