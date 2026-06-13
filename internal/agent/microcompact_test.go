package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestMicrocompact_OffloadsLargeBlocks — the basic contract: a
// tool_result over MicrocompactMinChars gets written to disk and
// replaced inline with a recoverable stub.
func TestMicrocompact_OffloadsLargeBlocks(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 2
	cfg.MicrocompactDir = dir
	cfg.MicrocompactMinChars = 100
	cfg.KeepRecentToolResults = 0 // isolate the size-based offload path
	c := NewCompactor(cfg, "test", 1000, &fakeSummarizer{})

	bigPayload := strings.Repeat("A", 500)
	msgs := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("toolu_001", "Bash"),
		toolResultMsg("toolu_001", bigPayload),
		msg(llm.RoleUser, "tail-1"),
		msg(llm.RoleAssistant, "tail-2"),
	}
	out := c.Microcompact(msgs)

	// Inline content replaced with stub.
	got := out[2].Content[0].ToolResult
	if strings.Contains(got, bigPayload) {
		t.Errorf("inline content should be replaced; still has full payload")
	}
	if !strings.Contains(got, "cached at") || !strings.Contains(got, "Read tool") {
		t.Errorf("stub should mention cache path + Read tool; got %q", got)
	}
	// Disk file exists with correct content.
	cached, err := os.ReadFile(filepath.Join(dir, "toolu_001.txt"))
	if err != nil {
		t.Fatalf("cached file missing: %v", err)
	}
	if string(cached) != bigPayload {
		t.Errorf("cached content mismatch: %d bytes vs %d", len(cached), len(bigPayload))
	}
}

// TestMicrocompact_SlashBearingToolUseID — a tool_use_id containing a
// slash (seen from MCP / OpenAI-compat gateways) must still offload.
// Pre-2026-06-13 the raw id made filepath.Join target a non-existent
// subdirectory, os.WriteFile failed with ENOENT, and the offload was
// silently skipped — the oversized result stayed in context. Now the
// id is sanitized identically to spill (spill.SanitizeID).
func TestMicrocompact_SlashBearingToolUseID(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 2
	cfg.MicrocompactDir = dir
	cfg.MicrocompactMinChars = 100
	cfg.KeepRecentToolResults = 0
	c := NewCompactor(cfg, "test", 1000, &fakeSummarizer{})

	bigPayload := strings.Repeat("B", 500)
	msgs := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("mcp/jira/search:42", "mcp__jira__search"),
		toolResultMsg("mcp/jira/search:42", bigPayload),
		msg(llm.RoleUser, "tail-1"),
		msg(llm.RoleAssistant, "tail-2"),
	}
	out := c.Microcompact(msgs)

	got := out[2].Content[0].ToolResult
	if strings.Contains(got, bigPayload) {
		t.Fatal("slash-bearing id: offload was silently skipped, full payload still inline")
	}
	if !strings.Contains(got, "cached at") {
		t.Errorf("expected a recoverable stub; got %q", got)
	}
	if !strings.Contains(got, dir) {
		t.Errorf("stub should reference the cache dir; got %q", got)
	}
}

// TestMicrocompact_DisabledWhenNoDir — no MicrocompactDir → no-op.
// Production-safe default for builds that haven't wired the cache path.
func TestMicrocompact_DisabledWhenNoDir(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.MicrocompactDir = ""
	cfg.MicrocompactMinChars = 100
	c := NewCompactor(cfg, "test", 1000, &fakeSummarizer{})

	huge := strings.Repeat("X", 5000)
	msgs := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("t1", "Bash"),
		toolResultMsg("t1", huge),
		msg(llm.RoleUser, "tail"),
	}
	out := c.Microcompact(msgs)
	if out[2].Content[0].ToolResult != huge {
		t.Errorf("disabled path should leave content untouched")
	}
}

// TestMicrocompact_RespectsProtectedTail — recent tool_results stay
// inline; only older ones get offloaded.
func TestMicrocompact_RespectsProtectedTail(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 2
	cfg.MicrocompactDir = dir
	cfg.MicrocompactMinChars = 100
	cfg.KeepRecentToolResults = 0 // isolate the ProtectLast-only path
	c := NewCompactor(cfg, "test", 1000, &fakeSummarizer{})

	big := strings.Repeat("Y", 500)
	msgs := []llm.Message{
		msg(llm.RoleUser, "seed"),
		toolUseMsg("old", "Bash"),
		toolResultMsg("old", big),
		toolUseMsg("recent", "Bash"),
		toolResultMsg("recent", big), // protected tail (last 2)
	}
	out := c.Microcompact(msgs)
	// Old result offloaded.
	if strings.Contains(out[2].Content[0].ToolResult, "YYYY") {
		t.Errorf("old result should be offloaded")
	}
	// Recent result intact.
	if out[4].Content[0].ToolResult != big {
		t.Errorf("recent result must stay inline; got len=%d", len(out[4].Content[0].ToolResult))
	}
}

// TestShouldMicrocompact_GatedByDir — without MicrocompactDir even an
// over-threshold convo returns false.
func TestShouldMicrocompact_GatedByDir(t *testing.T) {
	cfg := DefaultCompactionConfig()
	cfg.MicrocompactDir = ""
	cfg.MicrocompactMinChars = 100
	c := NewCompactor(cfg, "test", 1000, &fakeSummarizer{})
	huge := []llm.Message{msg(llm.RoleUser, strings.Repeat("a", 5000))}
	if c.ShouldMicrocompact(huge) {
		t.Errorf("ShouldMicrocompact must respect empty MicrocompactDir")
	}
}
