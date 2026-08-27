package builtin

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

// helper: build a Memory tool wired to a fresh on-disk MemoryManager.
// Returns the tool + the manager so callers can sanity-check writes
// landed in the same store BuildContext reads from (the whole point
// of bug #11's rewrite).
func newTestMemory(t *testing.T) (Memory, *memory.MemoryManager) {
	t.Helper()
	mm, err := memory.NewMemoryManager(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("NewMemoryManager: %v", err)
	}
	return NewMemory(permission.New(permission.ModeBypass), mm), mm
}

func TestMemory_AddPersistsToBlock(t *testing.T) {
	tool, mm := newTestMemory(t)
	res, err := tool.Execute(context.Background(), map[string]any{
		"action":  "add",
		"target":  "user",
		"content": "user prefers Chinese responses",
	})
	if err != nil || res.IsError {
		t.Fatalf("add: err=%v isError=%v out=%q", err, res.IsError, res.Output)
	}
	// Verify the same store BuildContext reads from has the new content.
	blk := mm.Core().GetBlock("user")
	if !strings.Contains(blk.Content, "Chinese responses") {
		t.Errorf("Block.Content lost the add: %q", blk.Content)
	}
	// And BuildContext (system-prompt path) renders it. This is the
	// whole disconnect bug — the original Memory tool wrote to a
	// different file and BuildContext never saw it.
	rendered := mm.BuildContext()
	if !strings.Contains(rendered, "Chinese responses") {
		t.Errorf("BuildContext didn't pick up Memory.add — disconnect regressed:\n%s", rendered)
	}
}

func TestMemory_AddAppendsToExisting(t *testing.T) {
	tool, mm := newTestMemory(t)
	for _, c := range []string{"first fact", "second fact"} {
		res, _ := tool.Execute(context.Background(), map[string]any{
			"action": "add", "target": "user", "content": c,
		})
		if res.IsError {
			t.Fatalf("add %q: %s", c, res.Output)
		}
	}
	blk := mm.Core().GetBlock("user")
	if !strings.Contains(blk.Content, "first fact") || !strings.Contains(blk.Content, "second fact") {
		t.Errorf("second add overwrote first: %q", blk.Content)
	}
}

func TestMemoryConcurrentManagersDoNotLoseAdds(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	first, err := memory.NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := memory.NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	tools := []Memory{
		NewMemory(permission.New(permission.ModeBypass), first),
		NewMemory(permission.New(permission.ModeBypass), second),
	}
	start := make(chan struct{})
	errCh := make(chan error, len(tools))
	var wg sync.WaitGroup
	for toolIndex, tool := range tools {
		toolIndex, tool := toolIndex, tool
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 12; i++ {
				fact := fmt.Sprintf("tool-%d-fact-%02d", toolIndex, i)
				result, err := tool.Execute(context.Background(), map[string]any{
					"action": "add", "target": "working", "content": fact,
				})
				if err != nil {
					errCh <- err
					return
				}
				if result.IsError {
					errCh <- fmt.Errorf("%s", result.Output)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	block, err := first.ReadCoreBlock("working")
	if err != nil {
		t.Fatal(err)
	}
	for toolIndex := range tools {
		for i := 0; i < 12; i++ {
			want := fmt.Sprintf("tool-%d-fact-%02d", toolIndex, i)
			if strings.Count(block.Content, want) != 1 {
				t.Fatalf("Memory tool lost or duplicated %q:\n%s", want, block.Content)
			}
		}
	}
}

func TestMemory_ReplaceFindsMatch(t *testing.T) {
	tool, mm := newTestMemory(t)
	mm.Core().UpdateBlock("user", "uses Python\nuses Vim")
	res, err := tool.Execute(context.Background(), map[string]any{
		"action": "replace", "target": "user",
		"match": "uses Vim", "content": "uses Neovim",
	})
	if err != nil || res.IsError {
		t.Fatalf("replace: %s", res.Output)
	}
	blk := mm.Core().GetBlock("user")
	if strings.Contains(blk.Content, "uses Vim") || !strings.Contains(blk.Content, "Neovim") {
		t.Errorf("replace didn't swap correctly: %q", blk.Content)
	}
}

func TestMemory_ReplaceMissingMatchErrors(t *testing.T) {
	tool, _ := newTestMemory(t)
	res, _ := tool.Execute(context.Background(), map[string]any{
		"action": "replace", "target": "user",
		"match": "nope", "content": "x",
	})
	if !res.IsError {
		t.Errorf("expected error when match not found, got %q", res.Output)
	}
}

func TestMemory_ReadEmptyBlock(t *testing.T) {
	tool, _ := newTestMemory(t)
	res, _ := tool.Execute(context.Background(), map[string]any{
		"action": "read", "target": "working",
	})
	if !strings.Contains(res.Output, "empty") {
		t.Errorf("read on empty block should say (empty); got %q", res.Output)
	}
}

func TestMemory_RejectUnknownTarget(t *testing.T) {
	tool, _ := newTestMemory(t)
	res, _ := tool.Execute(context.Background(), map[string]any{
		"action": "add", "target": "garbage", "content": "x",
	})
	if !res.IsError {
		t.Errorf("unknown target should error, got %q", res.Output)
	}
}

func TestMemoryArchiveCarriesSessionProvenanceAndDeletes(t *testing.T) {
	tool, mm := newTestMemory(t)
	const (
		sessionID = "memory-tool-session"
		fact      = "the project release train uses a unique cobalt canary"
	)
	tool = tool.WithSourceSessionIDFn(func() string { return sessionID })
	res, err := tool.Execute(context.Background(), map[string]any{
		"action": "archive", "memory_type": "project", "content": fact,
	})
	if err != nil || res.IsError {
		t.Fatalf("archive: err=%v isError=%v out=%q", err, res.IsError, res.Output)
	}
	passages, err := mm.Archival().Search(memory.SearchOptions{Query: "cobalt canary", Limit: 5})
	if err != nil || len(passages) != 1 {
		t.Fatalf("search archived passage: passages=%+v err=%v", passages, err)
	}
	if passages[0].SourceSessionID != sessionID || passages[0].Source != "memory-tool" {
		t.Fatalf("archive provenance=%+v", passages[0])
	}
	if err := mm.DeleteSession(sessionID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	passages, err = mm.Archival().Search(memory.SearchOptions{Query: "cobalt canary", Limit: 5})
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	if len(passages) != 0 {
		t.Fatalf("session-owned archive survived delete: %+v", passages)
	}
}

func TestMemoryGlobalCorePreferenceSurvivesSessionDelete(t *testing.T) {
	tool, mm := newTestMemory(t)
	tool = tool.WithSourceSessionIDFn(func() string { return "session-a" })
	res, err := tool.Execute(context.Background(), map[string]any{
		"action": "add", "target": "user", "content": "always answer in concise Chinese",
	})
	if err != nil || res.IsError {
		t.Fatalf("add global preference: err=%v result=%+v", err, res)
	}
	if err := mm.DeleteSession("session-a"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if got := mm.Core().GetBlock("user"); got == nil || !strings.Contains(got.Content, "concise Chinese") {
		t.Fatalf("global user preference was treated as session-owned: %+v", got)
	}
}

func TestMemorySearchUsesUnifiedTopicCorpus(t *testing.T) {
	tool, mm := newTestMemory(t)
	content := []byte("---\nname: Quartz preference\ndescription: workstation preference\ntype: user\n---\n\nThe Nebula Quartz workstation uses compact replies.\n")
	if err := mm.CommitTopic(context.Background(), memory.TopicMutation{
		Path: filepath.Join(mm.Root(), "user_quartz.md"), Content: content,
		Source: memory.TopicSource{SessionID: "topic-session"},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), map[string]any{
		"action": "search", "query": "Nebula Quartz", "limit": 5,
	})
	if err != nil || result.IsError || !strings.Contains(result.Output, "Nebula Quartz") {
		t.Fatalf("Memory search did not see topic: result=%+v err=%v", result, err)
	}
}

// TestMemory_NilManagerReturnsClearError covers the `metis tools`
// listing case where NewMemory(gate, nil) is registered just to show
// the capability — Execute must error gracefully, not nil-deref.
func TestMemory_NilManagerReturnsClearError(t *testing.T) {
	tool := NewMemory(permission.New(permission.ModeBypass), nil)
	res, _ := tool.Execute(context.Background(), map[string]any{
		"action": "add", "target": "user", "content": "x",
	})
	if !res.IsError {
		t.Errorf("nil manager should produce IsError, got %q", res.Output)
	}
	if !strings.Contains(res.Output, "manager not initialized") {
		t.Errorf("error should mention manager not initialized, got %q", res.Output)
	}
}
