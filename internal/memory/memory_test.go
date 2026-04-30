package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// ============================================================================
// Test Utilities
// ============================================================================

func TestCleanSlugWord(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello", "hello"},
		{"Hello", "hello"},
		{"HELLO", "hello"},
		{"hello123", "hello123"},
		{"hello-world", "helloworld"},
		{"hello.world", "helloworld"},
		{"hi", "hi"}, // 2 chars, not filtered
		{"!", ""},    // empty after filtering
		{"123", "123"},
		{"a1b2c3", "a1b2c3"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := cleanSlugWord(tc.input)
			if result != tc.expected {
				t.Errorf("cleanSlugWord(%q) = %q, want %q", tc.input, result, tc.expected)
			}
		})
	}
}

func TestGenerateSlugFromSummary(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)

	tests := []struct {
		name     string
		summary  string
		contains string
	}{
		{
			name:     "empty summary uses timestamp",
			summary:  "",
			contains: "1430", // HHMM format
		},
		{
			name:     "short summary",
			summary:  "hi",
			contains: "hi", // "hi" is kept, 2 chars meets minimum
		},
		{
			name:     "normal summary",
			summary:  "User asked about Go programming",
			contains: "user-asked-about-go-programming",
		},
		{
			name:     "summary with special chars",
			summary:  "Debugged API endpoint issue! Fixed the bug.",
			contains: "debugged-api-endpoint-issue-fixed-the",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := generateSlugFromSummary(tc.summary, baseTime)
			// For non-empty summaries, check if result contains expected substring
			if tc.summary != "" && !strings.Contains(result, tc.contains) {
				t.Errorf("generateSlugFromSummary(%q) = %q, want to contain %q", tc.summary, result, tc.contains)
			}
		})
	}
}

func tempDir(t *testing.T) string {
	tmp := t.TempDir()
	return tmp
}

// ============================================================================
// CoreMemory Tests
// ============================================================================

func TestNewCoreMemory(t *testing.T) {
	dir := tempDir(t)
	cm := NewCoreMemory(dir)

	if cm == nil {
		t.Fatal("NewCoreMemory returned nil")
	}

	blocks := cm.GetBlocks()
	if len(blocks) != 4 {
		t.Errorf("expected 4 blocks, got %d", len(blocks))
	}

	// Check default labels
	labels := make(map[string]bool)
	for _, b := range blocks {
		labels[b.Label] = true
	}
	for _, label := range []string{"user", "system", "working", "summary"} {
		if !labels[label] {
			t.Errorf("missing block label: %s", label)
		}
	}
}

func TestCoreMemory_GetBlock(t *testing.T) {
	dir := tempDir(t)
	cm := NewCoreMemory(dir)

	tests := []struct {
		label    string
		expected bool
	}{
		{"user", true},
		{"system", true},
		{"working", true},
		{"summary", true},
		{"nonexistent", false},
	}

	for _, tc := range tests {
		t.Run(tc.label, func(t *testing.T) {
			block := cm.GetBlock(tc.label)
			if tc.expected && block == nil {
				t.Errorf("GetBlock(%q) returned nil, expected block", tc.label)
			}
			if !tc.expected && block != nil {
				t.Errorf("GetBlock(%q) returned block, expected nil", tc.label)
			}
		})
	}
}

func TestCoreMemory_UpdateBlock(t *testing.T) {
	dir := tempDir(t)
	cm := NewCoreMemory(dir)

	// Update user block
	err := cm.UpdateBlock("user", "I prefer using VSCode")
	if err != nil {
		t.Fatalf("UpdateBlock failed: %v", err)
	}

	// Verify block was updated
	block := cm.GetBlock("user")
	if block == nil {
		t.Fatal("GetBlock(user) returned nil after update")
	}
	if block.Content != "I prefer using VSCode" {
		t.Errorf("block.Content = %q, want %q", block.Content, "I prefer using VSCode")
	}

	// Verify file was persisted
	path := filepath.Join(dir, "MEMORY.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read MEMORY.md: %v", err)
	}
	if !strings.Contains(string(data), "I prefer using VSCode") {
		t.Errorf("MEMORY.md doesn't contain expected content")
	}
}

func TestCoreMemory_UpdateBlock_Truncation(t *testing.T) {
	dir := tempDir(t)
	cm := NewCoreMemory(dir)

	// Get user block limit (should be 2200)
	block := cm.GetBlock("user")
	if block == nil {
		t.Fatal("GetBlock(user) returned nil")
	}
	limit := block.MaxChars

	// Create content exceeding limit
	longContent := strings.Repeat("a", limit+500)

	err := cm.UpdateBlock("user", longContent)
	if err != nil {
		t.Fatalf("UpdateBlock failed: %v", err)
	}

	// Verify truncated
	block = cm.GetBlock("user")
	if len(block.Content) > limit {
		t.Errorf("block.Content length %d exceeds limit %d", len(block.Content), limit)
	}
}

func TestCoreMemory_Render_FrozenSnapshot(t *testing.T) {
	dir := tempDir(t)

	// Pre-create memory file so Load() captures content
	memPath := filepath.Join(dir, "MEMORY.md")
	memContent := `═══════════════════════════════════════════════
USER [10% — 10/2200 chars]
═══════════════════════════════════════════════
I like cats
`
	if err := os.WriteFile(memPath, []byte(memContent), 0644); err != nil {
		t.Fatalf("failed to write MEMORY.md: %v", err)
	}

	// Create CoreMemory - Load() will capture snapshot from files
	cm := NewCoreMemory(dir)

	// Get initial snapshot
	initial := cm.GetSnapshot()
	if !strings.Contains(initial, "I like cats") {
		t.Errorf("initial snapshot missing content 'I like cats', got: %s", initial)
	}

	// Update block: as of bug-#11 fix the snapshot REFRESHES on
	// UpdateBlock (was previously frozen, which broke the
	// Memory-tool→BuildContext path so writes never reached the next
	// turn's system prompt). The frozen-within-a-turn invariant still
	// holds because UpdateBlock is only called from tool execution
	// (between iterations, not during).
	cm.UpdateBlock("user", "I like dogs")

	snapshot := cm.GetSnapshot()
	if !strings.Contains(snapshot, "I like dogs") {
		t.Errorf("snapshot should reflect the new UpdateBlock content; got: %s", snapshot)
	}
	if strings.Contains(snapshot, "I like cats") {
		t.Errorf("old content 'I like cats' should be gone from snapshot after replace; got: %s", snapshot)
	}
}

func TestCoreMemory_Render_ContextFencing(t *testing.T) {
	dir := tempDir(t)

	// Pre-create memory file
	memPath := filepath.Join(dir, "MEMORY.md")
	memContent := `═══════════════════════════════════════════════
USER [20% — 20/2200 chars]
═══════════════════════════════════════════════
My name is Alice
`
	if err := os.WriteFile(memPath, []byte(memContent), 0644); err != nil {
		t.Fatalf("failed to write MEMORY.md: %v", err)
	}

	cm := NewCoreMemory(dir)

	rendered := cm.Render()

	// Check for context fence tags
	if !strings.Contains(rendered, "<memory-context>") {
		t.Error("Render missing <memory-context> opening tag")
	}
	if !strings.Contains(rendered, "</memory-context>") {
		t.Error("Render missing </memory-context> closing tag")
	}
	if !strings.Contains(rendered, "System note:") {
		t.Error("Render missing system note")
	}
	if !strings.Contains(rendered, "My name is Alice") {
		t.Error("Render missing actual content")
	}
}

func TestCoreMemory_Render_EmptyContent(t *testing.T) {
	dir := tempDir(t)
	cm := NewCoreMemory(dir)

	// Empty memory should return empty string
	rendered := cm.Render()
	if rendered != "" {
		t.Errorf("Render() with empty blocks = %q, want empty string", rendered)
	}
}

func TestCoreMemory_Load(t *testing.T) {
	dir := tempDir(t)

	// Create MEMORY.md file manually
	memPath := filepath.Join(dir, "MEMORY.md")
	content := `═══════════════════════════════════════════════
USER [10% — 10/2200 chars]
═══════════════════════════════════════════════
I am testing the loading mechanism
`
	if err := os.WriteFile(memPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write MEMORY.md: %v", err)
	}

	// Create CoreMemory - should load from file
	cm := NewCoreMemory(dir)

	block := cm.GetBlock("user")
	if block == nil {
		t.Fatal("GetBlock(user) returned nil after Load")
	}
	if !strings.Contains(block.Content, "I am testing") {
		t.Errorf("loaded content = %q, want to contain 'I am testing'", block.Content)
	}
}

func TestCoreMemory_Save(t *testing.T) {
	dir := tempDir(t)
	cm := NewCoreMemory(dir)

	cm.UpdateBlock("system", "Metis is an AI assistant")

	// Verify file exists
	sysPath := filepath.Join(dir, "SYSTEM.md")
	if _, err := os.Stat(sysPath); os.IsNotExist(err) {
		t.Error("SYSTEM.md not created after Save")
	}
}

func TestCoreMemory_Freshness(t *testing.T) {
	dir := tempDir(t)
	cm := NewCoreMemory(dir)

	// Initially should have no memory files
	fresh := cm.Freshness()
	if fresh.Status != "no_memory_yet" {
		t.Errorf("freshness status = %q, want %q", fresh.Status, "no_memory_yet")
	}

	// Add content and save
	cm.UpdateBlock("user", "test content")

	// Now should be fresh
	fresh = cm.Freshness()
	if fresh.Status != "fresh" {
		t.Errorf("freshness status = %q, want %q", fresh.Status, "fresh")
	}
	if fresh.IsStale {
		t.Error("fresh memory should not be stale")
	}
}

func TestCoreMemory_SectionCount(t *testing.T) {
	dir := tempDir(t)
	cm := NewCoreMemory(dir)

	// Initially 0 non-empty sections
	if count := cm.SectionCount(); count != 0 {
		t.Errorf("SectionCount() = %d, want 0", count)
	}

	// Add content
	cm.UpdateBlock("user", "test")

	if count := cm.SectionCount(); count != 1 {
		t.Errorf("SectionCount() = %d, want 1", count)
	}

	cm.UpdateBlock("system", "another")

	if count := cm.SectionCount(); count != 2 {
		t.Errorf("SectionCount() = %d, want 2", count)
	}
}

func TestCoreMemory_Stats(t *testing.T) {
	dir := tempDir(t)
	cm := NewCoreMemory(dir)

	cm.UpdateBlock("user", "hello")

	stats := cm.Stats()

	if stats["user"].Used != 5 {
		t.Errorf("stats[user].Used = %d, want 5", stats["user"].Used)
	}
	if stats["user"].Limit != 2200 {
		t.Errorf("stats[user].Limit = %d, want 2200", stats["user"].Limit)
	}
	if stats["user"].Pct == 0 {
		t.Error("stats[user].Pct should not be 0")
	}
}

// ============================================================================
// ArchivalMemory Tests
// ============================================================================

func TestNewArchivalMemory(t *testing.T) {
	dir := tempDir(t)
	am, err := NewArchivalMemory(dir)
	if err != nil {
		t.Fatalf("NewArchivalMemory failed: %v", err)
	}
	if am == nil {
		t.Fatal("NewArchivalMemory returned nil")
	}
}

func TestArchivalMemory_Insert(t *testing.T) {
	dir := tempDir(t)
	am, err := NewArchivalMemory(dir)
	if err != nil {
		t.Fatalf("NewArchivalMemory failed: %v", err)
	}

	p := Passage{
		Content: "This is a test passage",
		Tags:    []string{"test", "example"},
	}

	err = am.Insert(p)
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	// Verify can search for it
	results, err := am.Search(SearchOptions{Query: "test passage"})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("Search returned %d results, want 1", len(results))
	}
}

func TestArchivalMemory_Insert_EmptyID(t *testing.T) {
	dir := tempDir(t)
	am, _ := NewArchivalMemory(dir)

	// Insert with empty ID
	p := Passage{Content: "test with auto-generated ID"}
	err := am.Insert(p)
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	// Search should find it - proving ID was generated and stored
	results, _ := am.Search(SearchOptions{Query: "auto-generated"})
	if len(results) != 1 {
		t.Errorf("Search found %d results, want 1", len(results))
	}
	// ID should be non-empty in storage
	if results[0].ID == "" {
		t.Error("Stored passage should have non-empty ID")
	}
}

func TestArchivalMemory_Search_Query(t *testing.T) {
	dir := tempDir(t)
	am, _ := NewArchivalMemory(dir)

	am.Insert(Passage{Content: "apple banana cherry"})
	am.Insert(Passage{Content: "dog elephant fox"})
	am.Insert(Passage{Content: "apple pie"})

	results, _ := am.Search(SearchOptions{Query: "apple"})
	if len(results) != 2 {
		t.Errorf("Search(apple) returned %d results, want 2", len(results))
	}
}

func TestArchivalMemory_Search_Tags(t *testing.T) {
	dir := tempDir(t)
	am, _ := NewArchivalMemory(dir)

	am.Insert(Passage{Content: "test1", Tags: []string{"go", "test"}})
	am.Insert(Passage{Content: "test2", Tags: []string{"python"}})
	am.Insert(Passage{Content: "test3", Tags: []string{"go", "rust"}})

	results, _ := am.Search(SearchOptions{Tags: []string{"go"}})
	if len(results) != 2 {
		t.Errorf("Search(tags=go) returned %d results, want 2", len(results))
	}
}

func TestArchivalMemory_Search_Limit(t *testing.T) {
	dir := tempDir(t)
	am, _ := NewArchivalMemory(dir)

	for i := 0; i < 10; i++ {
		am.Insert(Passage{Content: "content"})
	}

	results, _ := am.Search(SearchOptions{Limit: 3})
	if len(results) != 3 {
		t.Errorf("Search(limit=3) returned %d results, want 3", len(results))
	}
}

func TestArchivalMemory_Stats(t *testing.T) {
	dir := tempDir(t)
	am, _ := NewArchivalMemory(dir)

	am.Insert(Passage{Content: "first"})
	am.Insert(Passage{Content: "second"})
	am.Insert(Passage{Content: "third"})

	stats := am.Stats()
	if stats.Count != 3 {
		t.Errorf("Stats.Count = %d, want 3", stats.Count)
	}
	if stats.Oldest == "" || stats.Newest == "" {
		t.Error("Stats timestamps should not be empty")
	}
}

// ============================================================================
// RecallMemory Tests
// ============================================================================

func TestNewRecallMemory(t *testing.T) {
	dir := tempDir(t)
	rm, err := NewRecallMemory(dir, 50)
	if err != nil {
		t.Fatalf("NewRecallMemory failed: %v", err)
	}
	if rm == nil {
		t.Fatal("NewRecallMemory returned nil")
	}
}

func TestRecallMemory_Add(t *testing.T) {
	dir := tempDir(t)
	rm, _ := NewRecallMemory(dir, 50)

	rm.Add("user", "Hello")
	rm.Add("assistant", "Hi there!")

	msgs := rm.GetMessages()
	if len(msgs) != 2 {
		t.Errorf("GetMessages() returned %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "Hello" {
		t.Errorf("first message = %+v, want user/Hello", msgs[0])
	}
}

func TestRecallMemory_Add_Truncation(t *testing.T) {
	// RecallMemory.Add itself does NOT truncate - that's the caller's responsibility
	// (see MemoryManager.OnTurnEnd which calls truncate() before Add)
	// This test verifies Add stores whatever it receives

	dir := tempDir(t)
	rm, _ := NewRecallMemory(dir, 50)

	longContent := strings.Repeat("x", 1000)
	rm.Add("user", longContent)

	msgs := rm.GetMessages()
	// Content should be stored as-is (no truncation in Add itself)
	if len(msgs[0].Content) != 1000 {
		t.Errorf("message should be stored as 1000 chars, got %d", len(msgs[0].Content))
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		max      int
		expected string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 3, "hel"},
		{"", 5, ""},
		{"hello world", 8, "hello wo"},
	}

	for _, tc := range tests {
		result := truncate(tc.input, tc.max)
		if result != tc.expected {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.input, tc.max, result, tc.expected)
		}
	}
}

func TestRecallMemory_ShouldSummarize(t *testing.T) {
	dir := tempDir(t)
	rm, _ := NewRecallMemory(dir, 5)

	if rm.ShouldSummarize() {
		t.Error("ShouldSummarize should be false when under limit")
	}

	// Add 5 messages
	for i := 0; i < 5; i++ {
		rm.Add("user", "message")
	}

	if !rm.ShouldSummarize() {
		t.Error("ShouldSummarize should be true when at limit")
	}
}

func TestRecallMemory_Summarize(t *testing.T) {
	dir := tempDir(t)
	rm, _ := NewRecallMemory(dir, 10)

	// Add some messages (4 total)
	rm.Add("user", "Hello")
	rm.Add("assistant", "Hi")
	rm.Add("user", "How are you?")
	rm.Add("assistant", "Fine thanks")

	initialCount := len(rm.GetMessages())
	if initialCount != 4 {
		t.Fatalf("expected 4 messages, got %d", initialCount)
	}

	// Summarize
	rm.Summarize("User greeted assistant and asked how it was doing")

	msgs := rm.GetMessages()
	// After summarize: 2 (kept from original) + 2 (system markers) = 4
	// So count stays same, but content is compressed
	if len(msgs) != initialCount {
		t.Errorf("after summarize, message count = %d, want %d (2 kept + 2 system)", len(msgs), initialCount)
	}

	// Check sessions were recorded
	stats := rm.Stats()
	if stats.Sessions != 1 {
		t.Errorf("Stats().Sessions = %d, want 1", stats.Sessions)
	}
}

func TestRecallMemory_Stats(t *testing.T) {
	dir := tempDir(t)
	rm, _ := NewRecallMemory(dir, 50)

	rm.Add("user", "first")
	rm.Add("assistant", "second")

	stats := rm.Stats()
	if stats.Messages != 2 {
		t.Errorf("Stats().Messages = %d, want 2", stats.Messages)
	}
	if stats.Oldest == "" || stats.Newest == "" {
		t.Error("Stats timestamps should not be empty")
	}
}

// ============================================================================
// DailyStore Tests
// ============================================================================

func TestNewDailyStore(t *testing.T) {
	dir := tempDir(t)
	ds, err := NewDailyStore(dir)
	if err != nil {
		t.Fatalf("NewDailyStore failed: %v", err)
	}
	if ds == nil {
		t.Fatal("NewDailyStore returned nil")
	}
}

func TestDailyStore_Save(t *testing.T) {
	dir := tempDir(t)
	ds, _ := NewDailyStore(dir)

	err := ds.Save("session-123", "new", "User discussed Go programming")
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Check file was created
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			found = true
			break
		}
	}
	if !found {
		t.Error("no .md file created in daily store")
	}
}

func TestDailyStore_Save_EmptySummary(t *testing.T) {
	dir := tempDir(t)
	ds, _ := NewDailyStore(dir)

	// Save with empty summary should still work (uses timestamp slug)
	err := ds.Save("session-456", "reset", "")
	if err != nil {
		t.Fatalf("Save with empty summary failed: %v", err)
	}

	entries, _ := ds.List(10)
	if len(entries) != 1 {
		t.Errorf("List returned %d entries, want 1", len(entries))
	}
}

func TestDailyStore_List(t *testing.T) {
	dir := tempDir(t)
	ds, _ := NewDailyStore(dir)

	// Create files manually for known dates
	ds.Save("s1", "new", "First session content")
	time.Sleep(10 * time.Millisecond) // Ensure different timestamps
	ds.Save("s2", "new", "Second session content")

	notes, err := ds.List(10)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(notes) != 2 {
		t.Errorf("List returned %d notes, want 2", len(notes))
	}
}

func TestDailyStore_List_Limit(t *testing.T) {
	dir := tempDir(t)
	ds, _ := NewDailyStore(dir)

	// Create multiple notes - ensure each gets a unique slug by using distinct content
	contents := []string{
		"alpha programming session",
		"beta debugging session",
		"gamma code review",
		"delta feature work",
		"epsilon testing session",
	}
	for i, content := range contents {
		ds.Save("session-"+string(rune('a'+i)), "new", content)
		// Wait between saves so timestamps differ
		time.Sleep(100 * time.Millisecond)
	}

	notes, _ := ds.List(3)
	if len(notes) != 3 {
		t.Errorf("List(limit=3) returned %d notes, want 3 (notes: %+v)", len(notes), notes)
	}
}

func TestDailyStore_List_Empty(t *testing.T) {
	dir := tempDir(t)
	ds, _ := NewDailyStore(dir)

	notes, err := ds.List(10)
	if err != nil {
		t.Fatalf("List on empty store failed: %v", err)
	}
	// List returns nil slice for empty dir
	if notes != nil && len(notes) != 0 {
		t.Errorf("List on empty store returned %d notes, want 0 or nil", len(notes))
	}
}

// ============================================================================
// MemoryManager Integration Tests
// ============================================================================

func TestNewMemoryManager(t *testing.T) {
	dir := tempDir(t)
	mm, err := NewMemoryManager(dir)
	if err != nil {
		t.Fatalf("NewMemoryManager failed: %v", err)
	}
	if mm == nil {
		t.Fatal("NewMemoryManager returned nil")
	}
}

func TestMemoryManager_BuildContext(t *testing.T) {
	dir := tempDir(t)

	// Pre-create memory file so snapshot is captured at Load()
	memRoot := filepath.Join(dir, "core.d")
	if err := os.MkdirAll(memRoot, 0755); err != nil {
		t.Fatalf("failed to create core.d: %v", err)
	}
	memPath := filepath.Join(memRoot, "MEMORY.md")
	memContent := `═══════════════════════════════════════════════
USER [20% — 20/2200 chars]
═══════════════════════════════════════════════
I love coding
`
	if err := os.WriteFile(memPath, []byte(memContent), 0644); err != nil {
		t.Fatalf("failed to write MEMORY.md: %v", err)
	}

	mm, _ := NewMemoryManager(dir)

	// BuildContext should return content with fence tags
	ctx := mm.BuildContext()
	if !strings.Contains(ctx, "I love coding") {
		t.Errorf("BuildContext missing content, got: %s", ctx)
	}
	if !strings.Contains(ctx, "<memory-context>") {
		t.Error("BuildContext missing fence tags")
	}
}

func TestMemoryManager_SaveDailyNote(t *testing.T) {
	dir := tempDir(t)
	mm, _ := NewMemoryManager(dir)

	err := mm.SaveDailyNote("test-session", "new", "Testing daily notes")
	if err != nil {
		t.Fatalf("SaveDailyNote failed: %v", err)
	}

	notes, _ := mm.ListDailyNotes(10)
	if len(notes) != 1 {
		t.Errorf("ListDailyNotes returned %d notes, want 1", len(notes))
	}
}

func TestMemoryManager_Freshness(t *testing.T) {
	dir := tempDir(t)
	mm, _ := NewMemoryManager(dir)

	// Initially no memory
	fresh := mm.Freshness()
	if fresh.Status != "no_memory_yet" {
		t.Errorf("freshness status = %q, want %q", fresh.Status, "no_memory_yet")
	}

	// Add content
	mm.core.UpdateBlock("user", "test")

	fresh = mm.Freshness()
	if fresh.Status != "fresh" {
		t.Errorf("freshness status = %q, want %q", fresh.Status, "fresh")
	}
}

func TestMemoryManager_OnTurnEnd(t *testing.T) {
	dir := tempDir(t)
	mm, _ := NewMemoryManager(dir)

	mm.OnTurnEnd(nil, "Hello assistant", "Hello user!")

	stats := mm.recall.Stats()
	if stats.Messages != 2 {
		t.Errorf("recall.Messages = %d, want 2", stats.Messages)
	}
}

// ============================================================================
// JSON Parsing Regression Test (P0 fix)
// ============================================================================

func TestArchivalMemory_JSONContentWithPipes(t *testing.T) {
	dir := tempDir(t)
	am, _ := NewArchivalMemory(dir)

	// Content with pipe characters - this used to break old parsing
	contentWithPipes := "apple | banana | cherry | date"
	am.Insert(Passage{Content: contentWithPipes})

	results, err := am.Search(SearchOptions{Query: "banana"})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("Search returned %d results, want 1", len(results))
	}
	if results[0].Content != contentWithPipes {
		t.Errorf("Content = %q, want %q", results[0].Content, contentWithPipes)
	}
}

func TestArchivalMemory_JSONContentWithNewlines(t *testing.T) {
	dir := tempDir(t)
	am, _ := NewArchivalMemory(dir)

	// Content with newlines
	multilineContent := "line1\nline2\nline3"
	am.Insert(Passage{Content: multilineContent})

	results, _ := am.Search(SearchOptions{Query: "line2"})
	if len(results) != 1 {
		t.Errorf("Search returned %d results, want 1", len(results))
	}
}

func TestArchivalMemory_JSONContentWithQuotes(t *testing.T) {
	dir := tempDir(t)
	am, _ := NewArchivalMemory(dir)

	// Content with JSON-special chars
	quotedContent := `He said "hello" and then 'bye'`
	am.Insert(Passage{Content: quotedContent})

	results, _ := am.Search(SearchOptions{Query: "hello"})
	if len(results) != 1 {
		t.Errorf("Search returned %d results, want 1", len(results))
	}
}

// TestArchivalSearch_TypeFilter locks in the new SearchOptions.Types
// behavior added in Phase 2.1: a populated Types slice restricts
// results to passages whose Type matches one of the listed values.
// Empty/missing Type on a passage is treated as TypeContext.
func TestArchivalSearch_TypeFilter(t *testing.T) {
	dir := tempDir(t)
	am, _ := NewArchivalMemory(dir)
	am.Insert(Passage{Content: "user prefers Chinese", Type: TypeUser})
	am.Insert(Passage{Content: "this repo uses Go 1.26", Type: TypeProject})
	am.Insert(Passage{Content: "general note about today"}) // empty Type = context

	t.Run("filter to user only", func(t *testing.T) {
		res, _ := am.Search(SearchOptions{Types: []string{TypeUser}})
		if len(res) != 1 || res[0].Type != TypeUser {
			t.Errorf("expected 1 user passage, got %+v", res)
		}
	})
	t.Run("filter to multiple types", func(t *testing.T) {
		res, _ := am.Search(SearchOptions{Types: []string{TypeUser, TypeProject}})
		if len(res) != 2 {
			t.Errorf("expected 2 passages (user+project), got %d", len(res))
		}
	})
	t.Run("filter to context catches empty type", func(t *testing.T) {
		res, _ := am.Search(SearchOptions{Types: []string{TypeContext}})
		if len(res) != 1 || res[0].Content != "general note about today" {
			t.Errorf("expected the type-less passage to match TypeContext filter; got %+v", res)
		}
	})
}

// TestDailyStore_RecentSummary covers Phase 2.2: the cross-session
// daily reverse load. Save 3 notes (2 within window, 1 outside) and
// confirm RecentSummary returns the in-window summaries with their
// dates as section headers.
func TestDailyStore_RecentSummary(t *testing.T) {
	dir := tempDir(t)
	ds, err := NewDailyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Two recent notes (today + within 7 days) and one synthetic
	// older one — we write the older one by hand at a stale date so
	// the file scanner sees it but the cutoff in RecentSummary
	// excludes it.
	if err := ds.Save("sess-1", "new", "fixed login bug today"); err != nil {
		t.Fatal(err)
	}
	if err := ds.Save("sess-2", "reset", "discussed metis architecture"); err != nil {
		t.Fatal(err)
	}
	staleName := "1990-01-01-old-thing.md"
	stale := "# Session: 1990-01-01 ancient\n\n- **Session ID**: sess-old\n\nshould be filtered\n"
	if err := os.WriteFile(filepath.Join(dir, staleName), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ds.RecentSummary(7, 0)
	if got == "" {
		t.Fatal("RecentSummary returned empty even though we saved 2 fresh notes")
	}
	if !strings.Contains(got, "fixed login bug") || !strings.Contains(got, "metis architecture") {
		t.Errorf("RecentSummary missing fresh content:\n%s", got)
	}
	if strings.Contains(got, "should be filtered") {
		t.Errorf("RecentSummary leaked stale content past 7-day cutoff:\n%s", got)
	}
	if !strings.HasPrefix(got, "## Recent sessions") {
		t.Errorf("RecentSummary should start with section header; got %q", got[:50])
	}
}

// TestBuildContext_IncludesDailyRecentSummary covers the integration
// — calling MemoryManager.BuildContext after writing daily notes
// produces a system-prompt fragment that contains the recent summary.
// Pre-fix the daily store was a write-only tomb (Phase 2 Plan).
func TestBuildContext_IncludesDailyRecentSummary(t *testing.T) {
	dir := tempDir(t)
	mm, err := NewMemoryManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Save a daily note via the manager facade.
	if err := mm.SaveDailyNote("sess-x", "new", "metis daily reverse load test"); err != nil {
		t.Fatal(err)
	}
	ctx := mm.BuildContext()
	if !strings.Contains(ctx, "metis daily reverse load test") {
		t.Errorf("BuildContext didn't pick up the daily note we just saved:\n%s", ctx)
	}
	if !strings.Contains(ctx, "Recent sessions") {
		t.Errorf("BuildContext should advertise the daily section header; got:\n%s", ctx)
	}
}

// TestParseDistilled covers the JSON extraction logic for the auto-
// distillation pipeline (Phase 2.3). LLMs frequently wrap arrays in
// markdown fences or sneak in commentary, so the parser is
// deliberately tolerant.
func TestParseDistilled(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int // expected fact count
	}{
		{
			name: "clean JSON",
			raw:  `[{"type":"user","content":"prefers Chinese"}]`,
			want: 1,
		},
		{
			name: "markdown fences",
			raw:  "```json\n[{\"type\":\"feedback\",\"content\":\"don't force push\"}]\n```",
			want: 1,
		},
		{
			name: "leading commentary",
			raw:  "Here are the durable facts:\n[{\"type\":\"project\",\"content\":\"uses Go 1.26\"}]",
			want: 1,
		},
		{
			name: "empty array",
			raw:  "[]",
			want: 0,
		},
		{
			name: "malformed JSON",
			raw:  "{this is not json}",
			want: 0,
		},
		{
			name: "multiple facts with tags",
			raw:  `[{"type":"user","content":"a","tags":["x","y"]},{"type":"feedback","content":"b"}]`,
			want: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &llm.Response{
				Content: []llm.ContentBlock{{Type: "text", Text: tc.raw}},
			}
			got := parseDistilled(resp)
			if len(got) != tc.want {
				t.Errorf("parseDistilled(%q) = %d facts, want %d (%+v)",
					tc.raw, len(got), tc.want, got)
			}
		})
	}
}
