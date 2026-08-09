package tui

// vision_gate_test.go — covers the vision preflight. Low-level splitting is
// retained for callers that need it, but handleSubmit must not strip an image
// and send only its placeholder to a text-only model.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
	pubprov "github.com/Ricardo-M-L/metis/pkg/provider"
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
	m.cfg.Provider.Custom = map[string]config.ProviderRaw{
		"kimi": {
			Transport: "openai_chat",
			APIKey:    "test-key",
			BaseURL:   "http://127.0.0.1:1",
			Model:     "kimi-k3",
		},
	}
	m.providerName = "ark"
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
	if m.activeScreen == nil || !m.imageRecoveryPending {
		t.Fatal("text-only preflight should immediately open the vision-model recovery picker")
	}
	if view := m.activeScreen.View(); !strings.Contains(view, "Choose a vision model") || !strings.Contains(view, "kimi-k3") {
		t.Fatalf("vision recovery picker missing purpose/candidate:\n%s", view)
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "warning" || !strings.Contains(last.Content, "text-only") || !strings.Contains(last.Content, "kept") || !strings.Contains(last.Content, "choose") {
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

	// Apply the only configured vision candidate. The original prompt and
	// side-table attachment stay in place; no copy/cut/paste detour.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.activeScreen != nil || m.imageRecoveryPending {
		t.Fatalf("completed recovery picker did not close: screen=%T pending=%v", m.activeScreen, m.imageRecoveryPending)
	}
	if m.providerName != "kimi" || m.loop.Model != "kimi-k3" || !pubprov.ProviderSupportsVision(m.loop.Provider) {
		t.Fatalf("vision switch did not rebuild provider: profile=%q model=%q provider=%T", m.providerName, m.loop.Model, m.loop.Provider)
	}
	if got := m.input.Value(); got != "请看 [Image #1]" {
		t.Fatalf("vision switch lost original draft: %q", got)
	}
	if m.imagePaste[1] != path || m.imageCounter != 1 {
		t.Fatalf("vision switch lost pending attachment: map=%v counter=%d", m.imagePaste, m.imageCounter)
	}
	last = m.messages[len(m.messages)-1]
	if last.Role != "success" || !strings.Contains(last.Content, "press Enter to send") {
		t.Fatalf("missing post-switch resend guidance: %+v", last)
	}

	// One Enter now sends the exact original prompt + bytes.
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

func TestVisionRecoveryCancelKeepsDraftAndImages(t *testing.T) {
	m := newSlashTestModel(t)
	m.cfg.Provider.Custom = map[string]config.ProviderRaw{
		"kimi": {Transport: "openai_chat", APIKey: "test", Model: "kimi-k3"},
	}
	path := makeTinyPNG(t, t.TempDir(), "cancel.png", 8, 8)
	m.imagePaste = map[int]string{1: path}
	m.imageCounter = 1
	m.input.SetValue("保留 [Image #1]")

	_, _ = m.handleSubmit()
	if m.activeScreen == nil {
		t.Fatal("expected vision recovery picker")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.activeScreen != nil || m.imageRecoveryPending {
		t.Fatalf("cancel did not close recovery state: screen=%T pending=%v", m.activeScreen, m.imageRecoveryPending)
	}
	if got := m.input.Value(); got != "保留 [Image #1]" || m.imagePaste[1] != path || m.imageCounter != 1 {
		t.Fatalf("cancel lost draft/attachment: input=%q map=%v counter=%d", got, m.imagePaste, m.imageCounter)
	}
	last := m.messages[len(m.messages)-1]
	if !strings.Contains(last.Content, "cancelled") || !strings.Contains(last.Content, "kept") {
		t.Fatalf("cancel message is not actionable: %+v", last)
	}
}

func TestVisionRecoveryNoConfiguredCandidateKeepsEditor(t *testing.T) {
	m := newSlashTestModel(t)
	m.cfg.Provider.Custom = map[string]config.ProviderRaw{
		"ark": {Transport: "openai_chat", APIKey: "test", Model: "ark-code-latest"},
	}
	path := makeTinyPNG(t, t.TempDir(), "no-candidate.png", 8, 8)
	m.imagePaste = map[int]string{1: path}
	m.imageCounter = 1
	m.input.SetValue("查看 [Image #1]")

	_, _ = m.handleSubmit()
	if m.activeScreen != nil || m.imageRecoveryPending {
		t.Fatalf("no-candidate case should not open an empty picker: screen=%T pending=%v", m.activeScreen, m.imageRecoveryPending)
	}
	if got := m.input.Value(); got != "查看 [Image #1]" || m.imagePaste[1] != path {
		t.Fatalf("no-candidate case lost draft/attachment: input=%q map=%v", got, m.imagePaste)
	}
	last := m.messages[len(m.messages)-1]
	if !strings.Contains(last.Content, "no configured vision-capable") {
		t.Fatalf("missing no-candidate guidance: %+v", last)
	}
}

func TestVisionRecoveryMergedDefaultsWithoutCredentialsAreNotCandidates(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_TEST_NO_ANTHROPIC_KEY", "")
	t.Setenv("METIS_TEST_NO_OPENAI_KEY", "")
	m := newSlashTestModel(t)
	// Loaded config always carries these model defaults. They are not usable
	// recovery profiles unless a credential resolves.
	m.cfg.Provider.Anthropic = config.ProviderAnthropic{
		APIKeyEnv: "METIS_TEST_NO_ANTHROPIC_KEY", Model: "claude-sonnet-4-6",
	}
	m.cfg.Provider.OpenAI = config.ProviderOpenAI{
		APIKeyEnv: "METIS_TEST_NO_OPENAI_KEY", Model: "gpt-4o",
	}
	path := makeTinyPNG(t, t.TempDir(), "defaults-no-key.png", 8, 8)
	m.imagePaste = map[int]string{1: path}
	m.imageCounter = 1
	m.input.SetValue("查看 [Image #1]")

	_, _ = m.handleSubmit()
	if m.activeScreen != nil || m.imageRecoveryPending || m.imageRecoveryImageCount != 0 {
		t.Fatalf("credential-less defaults opened recovery: screen=%T pending=%v count=%d",
			m.activeScreen, m.imageRecoveryPending, m.imageRecoveryImageCount)
	}
	if last := m.messages[len(m.messages)-1]; !strings.Contains(last.Content, "no configured vision-capable") {
		t.Fatalf("missing no-credential recovery guidance: %+v", last)
	}
}

func TestModelChoiceHasCredentialsUsesCloudTransportAuth(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	saPath := filepath.Join(t.TempDir(), "service-account.json")
	if err := os.WriteFile(saPath, []byte(`{"type":"service_account"}`), 0o600); err != nil {
		t.Fatalf("write service-account fixture: %v", err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA_METIS_TEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "metis-test-secret")

	cfg := &config.Config{}
	cfg.Provider.Custom = map[string]config.ProviderRaw{
		"vertex": {
			Transport:          "vertex_anthropic",
			ServiceAccountFile: saPath,
			Model:              "claude-sonnet-4-6",
		},
		"bedrock": {
			Transport: "bedrock_anthropic",
			Model:     "us.anthropic.claude-sonnet-4-6-v1:0",
		},
	}

	for _, choice := range []screen.ModelChoice{
		{Provider: "vertex", ID: "claude-sonnet-4-6"},
		{Provider: "bedrock", ID: "us.anthropic.claude-sonnet-4-6-v1:0"},
	} {
		if !modelChoiceHasCredentials(cfg, choice) {
			t.Errorf("cloud profile %q was hidden despite valid transport credentials", choice.Provider)
		}
	}

	missingVertex := cfg.Provider.Custom["vertex"]
	missingVertex.ServiceAccountFile = filepath.Join(t.TempDir(), "missing.json")
	cfg.Provider.Custom["vertex"] = missingVertex
	if modelChoiceHasCredentials(cfg, screen.ModelChoice{Provider: "vertex", ID: missingVertex.Model}) {
		t.Error("Vertex profile with a missing service-account file was offered")
	}

	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	if modelChoiceHasCredentials(cfg, screen.ModelChoice{Provider: "bedrock", ID: cfg.Provider.Custom["bedrock"].Model}) {
		t.Error("Bedrock profile with only the AWS access key was offered")
	}
}

func TestVisionRecoveryReportsExactAtFileImageCount(t *testing.T) {
	m := newSlashTestModel(t)
	m.cfg.Provider.Custom = map[string]config.ProviderRaw{
		"kimi": {Transport: "openai_chat", APIKey: "test", BaseURL: "http://127.0.0.1:1", Model: "kimi-k3"},
	}
	path := makeTinyPNG(t, t.TempDir(), "at-file.png", 8, 8)
	m.input.SetValue("请看 @" + path)

	_, _ = m.handleSubmit()
	if m.imageRecoveryImageCount != 1 {
		t.Fatalf("@file recovery count=%d, want 1", m.imageRecoveryImageCount)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	last := m.messages[len(m.messages)-1]
	if last.Role != "success" || !strings.Contains(last.Content, "1 image(s) kept") {
		t.Fatalf("@file success reported wrong image count: %+v", last)
	}
}

func TestVisionRecoveryCountsOnlyReferencedPastedImages(t *testing.T) {
	m := newSlashTestModel(t)
	m.cfg.Provider.Custom = map[string]config.ProviderRaw{
		"kimi": {Transport: "openai_chat", APIKey: "test", BaseURL: "http://127.0.0.1:1", Model: "kimi-k3"},
	}
	dir := t.TempDir()
	m.imagePaste = map[int]string{
		1: makeTinyPNG(t, dir, "used.png", 8, 8),
		2: makeTinyPNG(t, dir, "deleted-placeholder.png", 8, 8),
	}
	m.imageCounter = 2
	m.input.SetValue("只看 [Image #1]")

	_, _ = m.handleSubmit()
	if m.imageRecoveryImageCount != 1 {
		t.Fatalf("recovery counted cached-but-unreferenced image: %d", m.imageRecoveryImageCount)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if last := m.messages[len(m.messages)-1]; !strings.Contains(last.Content, "1 image(s) kept") {
		t.Fatalf("pasted-image success reported wrong count: %+v", last)
	}
}

func TestMidTurnImageIsKeptInsteadOfSteeredAsPlaceholder(t *testing.T) {
	m := newSlashTestModel(t)
	m.cfg.Provider.Custom = map[string]config.ProviderRaw{
		"kimi": {Transport: "openai_chat", APIKey: "test", BaseURL: "http://127.0.0.1:1", Model: "kimi-k3"},
	}
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
	last := m.messages[len(m.messages)-1]
	if strings.Contains(last.Content, "copy/cut") || !strings.Contains(last.Content, "press Enter again") {
		t.Fatalf("mid-turn image warning still advertises the old recovery detour: %+v", last)
	}

	// Once the running turn is done, the promised next Enter must open the
	// same lossless vision picker used by an ordinary text-only submit.
	m.turnActive = false
	_, _ = m.handleSubmit()
	if m.activeScreen == nil || !m.imageRecoveryPending || m.imageRecoveryImageCount != 1 {
		t.Fatalf("post-turn Enter did not open recovery picker: screen=%T pending=%v count=%d",
			m.activeScreen, m.imageRecoveryPending, m.imageRecoveryImageCount)
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
