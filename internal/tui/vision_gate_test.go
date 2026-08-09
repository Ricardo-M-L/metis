package tui

// vision_gate_test.go — covers the vision preflight. Low-level splitting is
// retained for callers that need it, but handleSubmit must not strip an image
// and send only its placeholder to a text-only model.

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

type visionFakeProvider struct{ fakeProvider }

func (visionFakeProvider) SupportsVision() bool { return true }

func TestSplitOffImageBlocks_StripsImagesOnly(t *testing.T) {
	t.Parallel()
	blocks := []llm.ContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "image", MediaType: "image/png", Data: "..."},
		{Type: "text", Text: " world"},
		{Type: "image", MediaType: "image/jpeg", Data: "..."},
	}
	stripped, kept := splitOffImageBlocks(blocks)
	if stripped != 2 {
		t.Errorf("stripped count = %d, want 2", stripped)
	}
	if len(kept) != 2 {
		t.Fatalf("kept = %d, want 2 (text only)", len(kept))
	}
	for i, b := range kept {
		if b.Type != "text" {
			t.Errorf("kept[%d].Type = %q, want text", i, b.Type)
		}
	}
}

func TestSubmitImageToTextOnlyModelKeepsPromptAndAttachment(t *testing.T) {
	m := newSlashTestModel(t)
	m.loop.Model = "deepseek-v4-flash"
	m.model = "deepseek-v4-flash"
	path := makeTinyPNG(t, t.TempDir(), "kept.png", 8, 8)
	m.imagePaste = map[int]string{1: path}
	m.imageCounter = 1
	m.input.SetValue("请看 [Image #1]")

	_, cmd := m.handleSubmit()
	if cmd != nil {
		t.Fatal("text-only image preflight must not start an LLM turn")
	}
	if got := m.input.Value(); got != "请看 [Image #1]" {
		t.Fatalf("input was not preserved: %q", got)
	}
	if got := m.imagePaste[1]; got != path || m.imageCounter != 1 {
		t.Fatalf("cached attachment was lost: map=%v counter=%d", m.imagePaste, m.imageCounter)
	}
	if hist := m.loop.History(); len(hist) != 0 {
		t.Fatalf("placeholder-only message leaked to model history: %+v", hist)
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "warning" || !strings.Contains(last.Content, "text-only") || !strings.Contains(last.Content, "kept") {
		t.Fatalf("missing actionable preflight warning: %+v", last)
	}
	// Key repeat / impatient Enter must not grow an identical warning chain.
	before := len(m.messages)
	_, _ = m.handleSubmit()
	if len(m.messages) != before {
		t.Fatalf("repeated Enter duplicated image warning: before=%d after=%d", before, len(m.messages))
	}
	if got := m.input.Value(); got != "请看 [Image #1]" {
		t.Fatalf("second preflight corrupted restored placeholder: %q", got)
	}
	if hist := m.loop.History(); len(hist) != 0 {
		t.Fatalf("repeated Enter leaked placeholder to history: %+v", hist)
	}

	// The warning's documented recovery path must be true: replacing the
	// editor with /model does not discard the side-table attachment. After a
	// successful vision-provider switch, putting the copied prompt back sends
	// the original image bytes without re-pasting the image.
	m.input.SetValue("/model")
	_, _ = m.handleSubmit()
	if m.imagePaste[1] != path || m.imageCounter != 1 {
		t.Fatalf("/model discarded pending image attachment: map=%v counter=%d", m.imagePaste, m.imageCounter)
	}
	m.activeScreen = nil // stand in for completing the picker selection
	m.loop.Provider = visionFakeProvider{}
	m.loop.Model = "gpt-4o"
	m.model = "gpt-4o"
	m.input.SetValue("请看 [Image #1]") // paste the copied prompt back
	_, _ = m.handleSubmit()
	hist := m.loop.History()
	if len(hist) != 1 {
		t.Fatalf("recovered image submit history len=%d, want 1: %+v", len(hist), hist)
	}
	imageBlocks := 0
	for _, block := range hist[0].Content {
		if block.Type == "image" && block.Data != "" {
			imageBlocks++
		}
	}
	if imageBlocks != 1 {
		t.Fatalf("recovered prompt sent %d image blocks, want 1: %+v", imageBlocks, hist[0].Content)
	}
	if m.imagePaste != nil || m.imageCounter != 0 {
		t.Fatalf("successful image submit did not clear pending attachment: map=%v counter=%d", m.imagePaste, m.imageCounter)
	}
}

func TestMidTurnImageIsKeptInsteadOfSteeredAsPlaceholder(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	path := makeTinyPNG(t, t.TempDir(), "steer.png", 8, 8)
	m.imagePaste = map[int]string{1: path}
	m.imageCounter = 1
	m.input.SetValue("补充图片 [Image #1]")

	_, cmd := m.handleSubmit()
	if cmd != nil {
		t.Fatal("mid-turn image must wait instead of starting work")
	}
	if got := m.input.Value(); got != "补充图片 [Image #1]" {
		t.Fatalf("mid-turn image input was cleared: %q", got)
	}
	if m.imagePaste[1] != path {
		t.Fatalf("mid-turn image cache mapping was lost: %v", m.imagePaste)
	}
	if hist := m.loop.History(); len(hist) != 0 {
		t.Fatalf("raw image placeholder was steered into history: %+v", hist)
	}
}

func TestSubmitMissingCachedImageDoesNotLeakPathOrLoseAttachment(t *testing.T) {
	m := newSlashTestModel(t)
	m.loop.Provider = visionFakeProvider{}
	m.loop.Model = "gpt-4o"
	m.model = "gpt-4o"
	missing := t.TempDir() + "/clipboard-image.png"
	m.imagePaste = map[int]string{1: missing}
	m.imageCounter = 1
	m.input.SetValue("请看 [Image #1]")

	_, cmd := m.handleSubmit()
	if cmd != nil {
		t.Fatal("missing cached image must not start an LLM turn")
	}
	if got := m.input.Value(); got != "请看 [Image #1]" {
		t.Fatalf("input was not preserved: %q", got)
	}
	if got := m.imagePaste[1]; got != missing || m.imageCounter != 1 {
		t.Fatalf("missing attachment mapping was lost: map=%v counter=%d", m.imagePaste, m.imageCounter)
	}
	if hist := m.loop.History(); len(hist) != 0 {
		t.Fatalf("missing image path leaked to model history: %+v", hist)
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "warning" || !strings.Contains(last.Content, "image not sent") || !strings.Contains(last.Content, "kept") {
		t.Fatalf("missing actionable cache warning: %+v", last)
	}

	before := len(m.messages)
	_, _ = m.handleSubmit()
	if len(m.messages) != before {
		t.Fatalf("repeated Enter duplicated missing-image warning: before=%d after=%d", before, len(m.messages))
	}
}

func TestSplitOffImageBlocks_NoImages_NoChange(t *testing.T) {
	t.Parallel()
	blocks := []llm.ContentBlock{
		{Type: "text", Text: "hi"},
		{Type: "text", Text: " there"},
	}
	stripped, kept := splitOffImageBlocks(blocks)
	if stripped != 0 {
		t.Errorf("nothing to strip, got count = %d", stripped)
	}
	if len(kept) != len(blocks) {
		t.Errorf("kept len = %d, want %d", len(kept), len(blocks))
	}
}

func TestSplitOffImageBlocks_AllImages_LeavesEmpty(t *testing.T) {
	t.Parallel()
	blocks := []llm.ContentBlock{
		{Type: "image", MediaType: "image/png", Data: "a"},
		{Type: "image", MediaType: "image/jpeg", Data: "b"},
	}
	stripped, kept := splitOffImageBlocks(blocks)
	if stripped != 2 {
		t.Errorf("stripped = %d, want 2", stripped)
	}
	if len(kept) != 0 {
		t.Errorf("kept = %d, want 0 — only images present", len(kept))
	}
}
