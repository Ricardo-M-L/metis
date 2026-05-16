package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memdir"
)

func newTestExtractor(t *testing.T, prov llm.Provider, root string) (*Loop, *AutoMemoryExtractor) {
	t.Helper()
	reg := newRegistryWith(t)
	loop := NewLoop(prov, reg, nil, nil, "system", 10)
	loop.Model = "test"
	loop.AutoMemory = true
	ext, err := NewAutoMemoryExtractor(loop, root)
	if err != nil {
		t.Fatalf("NewAutoMemoryExtractor: %v", err)
	}
	loop.autoMemExtractor = ext

	// Phase A test-isolation: point the dream gate at a temp sessions
	// dir populated with enough mock JSONLs to clear gate 3. Without
	// this, tests would either (a) leak the user's real
	// ~/.metis/sessions state and intermittently fail when it's empty,
	// or (b) wedge behind the gate because t.TempDir() has no sessions.
	sessDir := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	for _, n := range []string{"s1", "s2", "s3", "s4"} {
		p := filepath.Join(sessDir, n+".jsonl")
		if err := os.WriteFile(p, []byte(`{"kind":"header"}`+"\n"), 0o644); err != nil {
			t.Fatalf("write mock session: %v", err)
		}
	}
	ext.SetSessionsDir(sessDir)
	return loop, ext
}

func TestAutoMemoryExtractor_DefaultRootCreated(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	prov := &scriptedProvider{}
	_, ext := newTestExtractor(t, prov, root)
	if ext.MemdirRoot() != root {
		t.Errorf("MemdirRoot = %q, want %q", ext.MemdirRoot(), root)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("memdir root not created: %v", err)
	}
}

func TestAutoMemoryExtractor_OnLoopEnd_SkipsWhenDisabled(t *testing.T) {
	prov := &scriptedProvider{}
	loop, ext := newTestExtractor(t, prov, t.TempDir())
	loop.AutoMemory = false
	ext.OnLoopEnd(context.Background(), "end_turn")
	if got := ext.Stats(); got.TotalExtractions != 0 || got.InProgress {
		t.Errorf("disabled should be a no-op; stats = %+v", got)
	}
}

func TestAutoMemoryExtractor_OnLoopEnd_IgnoresNonEndTurnStops(t *testing.T) {
	prov := &scriptedProvider{
		resps: []*llm.Response{{Content: []llm.ContentBlock{{Type: "text", Text: "ok"}}, StopReason: "end_turn"}},
	}
	_, ext := newTestExtractor(t, prov, t.TempDir())
	for _, stop := range []string{"max_iterations", "halted_by_hook", "loop_detected", "error"} {
		ext.OnLoopEnd(context.Background(), stop)
	}
	time.Sleep(50 * time.Millisecond)
	if got := ext.Stats(); got.TotalExtractions != 0 {
		t.Errorf("non-end-turn stops should not extract; got %+v", got)
	}
}

func TestAutoMemoryExtractor_OnLoopEnd_TriggersAndAdvancesCursor(t *testing.T) {
	prov := &scriptedProvider{
		resps: []*llm.Response{
			{Content: []llm.ContentBlock{{Type: "text", Text: "no new memos"}}, StopReason: "end_turn"},
		},
	}
	loop, ext := newTestExtractor(t, prov, t.TempDir())
	loop.AppendUser("hello")
	loop.mu.Lock()
	loop.Messages = append(loop.Messages, llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{{Type: "text", Text: "hi back"}},
	})
	loop.mu.Unlock()

	ext.OnLoopEnd(context.Background(), "end_turn")
	// The fork goroutine is async; wait for it.
	waitFor(t, time.Second, func() bool {
		return ext.Stats().TotalExtractions == 1
	})
	stats := ext.Stats()
	if stats.LastProcessedIdx != 2 {
		t.Errorf("LastProcessedIdx = %d, want 2", stats.LastProcessedIdx)
	}
}

func TestAutoMemoryExtractor_RateLimitsRapidLoopEnds(t *testing.T) {
	prov := &scriptedProvider{
		resps: []*llm.Response{
			{Content: []llm.ContentBlock{{Type: "text", Text: "x"}}, StopReason: "end_turn"},
			{Content: []llm.ContentBlock{{Type: "text", Text: "x"}}, StopReason: "end_turn"},
		},
	}
	_, ext := newTestExtractor(t, prov, t.TempDir())
	// Disk-backed dream gate is now the cross-process throttle; this
	// test isolates the in-memory MinInterval+Pending machinery, so we
	// bypass the gate (otherwise the second call would never reach the
	// in-memory rate limit — it'd bail at "too-soon").
	ext.setDreamGateBypass(true)

	ext.OnLoopEnd(context.Background(), "end_turn")
	waitFor(t, time.Second, func() bool { return ext.Stats().TotalExtractions == 1 })

	// Second call within MinInterval should NOT fire a 2nd extraction.
	ext.OnLoopEnd(context.Background(), "end_turn")
	time.Sleep(150 * time.Millisecond)
	if ext.Stats().TotalExtractions != 1 {
		t.Errorf("expected throttle, got %d extractions", ext.Stats().TotalExtractions)
	}
	if !ext.Stats().Pending {
		t.Errorf("expected Pending=true after throttle")
	}
}

func TestAutoMemoryExtractor_StashesPendingDuringInProgress(t *testing.T) {
	// blocking provider blocks first Complete forever.
	bp := &blockingProvider{release: make(chan struct{})}
	_, ext := newTestExtractor(t, bp, t.TempDir())
	// Bypass the disk gate so this test exercises the in-memory
	// in-progress / pending trailing-run machinery (see sibling
	// rate-limit test for the same reasoning).
	ext.setDreamGateBypass(true)
	ext.OnLoopEnd(context.Background(), "end_turn")
	// Wait until the goroutine entered the fork.
	waitFor(t, time.Second, func() bool { return ext.Stats().InProgress })

	// Second call must stash, not fire.
	ext.OnLoopEnd(context.Background(), "end_turn")
	if !ext.Stats().Pending {
		t.Errorf("expected pending=true after second call during in-progress")
	}
	close(bp.release)
}

func TestAutoMemoryExtractor_MutualExclusionWithMainAgent(t *testing.T) {
	prov := &scriptedProvider{
		resps: []*llm.Response{
			{Content: []llm.ContentBlock{{Type: "text", Text: "x"}}, StopReason: "end_turn"},
		},
	}
	root := t.TempDir()
	loop, ext := newTestExtractor(t, prov, root)
	// Main agent itself wrote to memdir.
	loop.AppendUser("hi")
	loop.mu.Lock()
	loop.Messages = append(loop.Messages, llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "u", ToolName: "Write",
			ToolInput: map[string]any{"file_path": filepath.Join(root, "user_role.md")},
		}},
	})
	loop.mu.Unlock()
	ext.OnLoopEnd(context.Background(), "end_turn")
	time.Sleep(100 * time.Millisecond)
	stats := ext.Stats()
	if stats.TotalExtractions != 0 {
		t.Errorf("should skip when main agent wrote memdir; got %+v", stats)
	}
	if stats.LastProcessedIdx == 0 {
		t.Errorf("cursor should still advance to skip these messages; got %+v", stats)
	}
}

func TestAutoMemoryExtractor_RegeneratesIndex(t *testing.T) {
	prov := &scriptedProvider{
		resps: []*llm.Response{
			{Content: []llm.ContentBlock{{Type: "text", Text: "wrote nothing this turn"}}, StopReason: "end_turn"},
		},
	}
	root := t.TempDir()
	// Pre-create a memdir file so regenerateIndex has something to write.
	bodyBytes, err := memdir.RenderFile(&memdir.Frontmatter{
		Name: "u", Description: "user role", Type: memdir.TypeUser,
	}, "body")
	if err != nil {
		t.Fatalf("RenderFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "user_role.md"), bodyBytes, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, ext := newTestExtractor(t, prov, root)
	ext.OnLoopEnd(context.Background(), "end_turn")
	// regenerateIndex runs after totalExtractions++; wait on the
	// post-condition (file existence), not the extractions counter.
	indexPath := memdir.IndexPath(root)
	waitFor(t, 2*time.Second, func() bool {
		_, err := os.Stat(indexPath)
		return err == nil
	})

	indexBytes, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("index not written: %v", err)
	}
	if !strings.Contains(string(indexBytes), "user_role.md") {
		t.Errorf("index missing memo: %q", indexBytes)
	}
}

func TestBuildExtractorPrompt_IncludesManifest(t *testing.T) {
	got := buildExtractorPrompt("/tmp/x", "## existing\n- one\n", nil)
	if !strings.Contains(got, "/tmp/x") {
		t.Errorf("prompt missing root: %q", got)
	}
	if !strings.Contains(got, "## existing") {
		t.Errorf("prompt missing manifest: %q", got)
	}
	if !strings.Contains(got, "Memory directory") {
		t.Errorf("prompt missing header: %q", got)
	}
}

// blockingProvider blocks Complete until release is closed. Used to
// exercise in-progress / pending state without timing flakiness.
type blockingProvider struct {
	release chan struct{}
	calls   int
}

func (p *blockingProvider) Name() string          { return "blocking" }
func (p *blockingProvider) MaxContextTokens() int { return 200_000 }
func (p *blockingProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	p.calls++
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: "x"}}, StopReason: "end_turn"}, nil
}
func (p *blockingProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, nil
}

// waitFor polls cond until true or timeout. Doesn't drift like sleep
// based timing; better for asserting on goroutine completion.
func waitFor(t *testing.T, max time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitFor: condition never met within %s", max)
}
