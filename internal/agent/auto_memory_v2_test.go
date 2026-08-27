package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memdir"
	"github.com/Ricardo-M-L/metis/internal/memory"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

func newTestExtractor(t *testing.T, prov llm.Provider, root string) (*Loop, *AutoMemoryExtractor) {
	t.Helper()
	reg := newRegistryWith(t)
	loop := NewLoop(prov, reg, nil, nil, "system", 10)
	loop.Model = "test"
	loop.AutoMemory = true
	ext, err := NewAutoMemoryExtractor(loop, root, "")
	if err != nil {
		t.Fatalf("NewAutoMemoryExtractor: %v", err)
	}
	// Test-isolate the skills dir too: the dream cycle reads/synthesizes/
	// curates here, so without this a dream run would touch (and the
	// curator could archive) the host's real ~/.metis/skills.
	ext.skillsDir = filepath.Join(t.TempDir(), "skills")
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
	// OnLoopEnd intentionally detaches its fork from the caller. Register this
	// after the fixture TempDirs so it runs first and joins any final index,
	// hygiene, curator, or dream-lock writes before those directories vanish.
	t.Cleanup(func() {
		waitFor(t, 2*time.Second, func() bool {
			return !ext.Stats().InProgress && ForkInflight() == 0
		})
	})
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
	// TotalExtractions advances before index regeneration and deferred
	// filesystem cleanup. Wait for the full fork lifecycle so TempDir cleanup
	// cannot race those final writes.
	waitFor(t, time.Second, func() bool {
		stats := ext.Stats()
		return stats.TotalExtractions == 1 && !stats.InProgress && ForkInflight() == 0
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
	waitFor(t, time.Second, func() bool {
		stats := ext.Stats()
		return stats.TotalExtractions == 1 && !stats.InProgress && ForkInflight() == 0
	})

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

func TestWaitAutoMemoryIdleFlushesThrottledPendingRun(t *testing.T) {
	provider := &scriptedProvider{resps: []*llm.Response{
		{Content: []llm.ContentBlock{{Type: "text", Text: "first"}}, StopReason: "end_turn"},
		{Content: []llm.ContentBlock{{Type: "text", Text: "second"}}, StopReason: "end_turn"},
	}}
	loop, ext := newTestExtractor(t, provider, t.TempDir())
	ext.setDreamGateBypass(true)
	loop.AppendUser("first completed turn")
	ext.OnLoopEnd(context.Background(), "end_turn")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := loop.WaitAutoMemoryIdle(ctx); err != nil {
		t.Fatalf("join first extraction: %v", err)
	}

	loop.AppendUser("latest turn inside throttle window")
	ext.OnLoopEnd(context.Background(), "end_turn")
	if stats := ext.Stats(); !stats.Pending || stats.InProgress {
		t.Fatalf("second turn was not pending-only: %+v", stats)
	}
	if err := loop.WaitAutoMemoryIdle(ctx); err != nil {
		t.Fatalf("flush pending extraction: %v", err)
	}
	if stats := ext.Stats(); stats.Pending || stats.InProgress || stats.TotalExtractions != 2 {
		t.Fatalf("shutdown join dropped pending extraction: %+v", stats)
	}
}

func TestWaitAutoMemoryIdleForcesTrailingRunUnderThrottle(t *testing.T) {
	provider := &blockingProvider{release: make(chan struct{})}
	loop, ext := newTestExtractor(t, provider, t.TempDir())
	ext.setDreamGateBypass(true)
	loop.AppendUser("first turn")
	ext.OnLoopEnd(context.Background(), "end_turn")
	waitFor(t, time.Second, func() bool { return ext.Stats().InProgress })

	loop.AppendUser("latest turn while first extraction runs")
	ext.OnLoopEnd(context.Background(), "end_turn")
	if !ext.Stats().Pending {
		t.Fatal("latest frozen turn was not queued")
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { done <- loop.WaitAutoMemoryIdle(ctx) }()
	waitFor(t, time.Second, func() bool {
		ext.mu.Lock()
		defer ext.mu.Unlock()
		return ext.flushPending
	})
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatalf("join forced trailing extraction: %v", err)
	}
	if stats := ext.Stats(); stats.Pending || stats.InProgress || stats.TotalExtractions != 2 {
		t.Fatalf("throttled trailing extraction was lost: %+v", stats)
	}
}

func TestAutoMemoryExtractor_InProgressStashesBeforeDurableDreamGate(t *testing.T) {
	provider := &gateOrderingProvider{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	loop, ext := newTestExtractor(t, provider, t.TempDir())
	// Deliberately do not call setDreamGateBypass: this regression must use
	// the production DreamLock path that moves its mtime to now while the
	// first extraction is in flight.
	loop.AppendUser("first completed turn")
	ext.OnLoopEnd(context.Background(), "end_turn")
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("first extraction did not reach provider")
	}
	waitFor(t, time.Second, func() bool {
		return ext.Stats().InProgress && !ext.Stats().Pending
	})

	const latest = "LATEST_PENDING_TURN_BEFORE_DURABLE_GATE"
	loop.AppendUser(latest)
	ext.OnLoopEnd(context.Background(), "end_turn")
	if stats := ext.Stats(); !stats.InProgress || !stats.Pending {
		t.Fatalf("latest turn was dropped by the durable gate: %+v", stats)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- loop.WaitAutoMemoryIdle(ctx) }()
	waitFor(t, time.Second, func() bool {
		ext.mu.Lock()
		defer ext.mu.Unlock()
		return ext.flushPending
	})
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatalf("flush pending extraction: %v", err)
	}
	if stats := ext.Stats(); stats.Pending || stats.InProgress || stats.TotalExtractions != 2 {
		t.Fatalf("pending production-gated extraction did not complete: %+v", stats)
	}

	requests := provider.requestsSnapshot()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want first run plus trailing run", len(requests))
	}
	if requestContainsText(requests[0], latest) {
		t.Fatal("first frozen extraction unexpectedly contains the later turn")
	}
	if !requestContainsText(requests[1], latest) {
		t.Fatal("trailing extraction did not receive the latest frozen turn")
	}
}

func TestAutoMemoryExtractor_StashesPendingDuringInProgress(t *testing.T) {
	// No prior test's detached fork may be part of this test's completion
	// boundary. Package tests are serial, so this drains earlier async work.
	waitFor(t, 2*time.Second, func() bool { return ForkInflight() == 0 })

	// blocking provider blocks first Complete forever.
	bp := &blockingProvider{release: make(chan struct{})}
	root := t.TempDir()
	_, ext := newTestExtractor(t, bp, root)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(bp.release) }) }
	// newTestExtractor registered several TempDir cleanups before this one.
	// Waiting here therefore drains background filesystem work before those
	// directories are removed, including on an earlier assertion failure.
	t.Cleanup(func() {
		release()
		waitFor(t, 2*time.Second, func() bool {
			return !ext.Stats().InProgress && ForkInflight() == 0
		})
	})
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
	release()
	// Idle is the lifecycle barrier: all post-extraction writes, including
	// dream-lock finalization, must finish before the test's TempDirs clean up.
	waitFor(t, 2*time.Second, func() bool {
		stats := ext.Stats()
		return !stats.InProgress && stats.TotalExtractions == 1 && ForkInflight() == 0
	})
	// Prove the completed boundary is safe for TempDir cleanup now, rather
	// than relying on testing.T to discover a late background writer.
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove completed extractor temp root: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("extractor temp root still exists after idle cleanup: %v", err)
	}
}

func TestAutoMemoryExtractor_OnLoopEndFreezesSessionHistoryAndProvenance(t *testing.T) {
	root := t.TempDir()
	memoPath := filepath.Join(root, "user_frozen_session.md")
	memo, err := memdir.RenderFile(&memdir.Frontmatter{
		Name: "frozen session", Description: "fact from session A", Type: memdir.TypeUser,
	}, "A_ONLY prefers frozen extraction snapshots.")
	if err != nil {
		t.Fatal(err)
	}
	provider := &sessionSwitchProvider{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		memoPath: memoPath,
		memo:     string(memo),
	}
	loop, ext := newTestExtractor(t, provider, root)
	loop.Registry.Register(autoMemoryWriteTool{})
	ext.setDreamGateBypass(true)

	var sessionID atomic.Value
	sessionID.Store("session-a")
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: sessionID.Load().(string)}
	}
	loop.AppendUser("A_ONLY history must be extracted")
	loop.mu.Lock()
	loop.Messages = append(loop.Messages, llm.Message{
		Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "A_ONLY acknowledged"}},
	})
	loop.mu.Unlock()

	// Keep the caller on one P until the session replacement is published. The
	// extractor is deliberately asynchronous, so this makes the historical bug
	// deterministic: a goroutine that snapshots after OnLoopEnd returns sees B.
	previousProcs := runtime.GOMAXPROCS(1)
	ext.OnLoopEnd(context.Background(), "end_turn")
	loop.ResetSession([]llm.Message{{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "B_ONLY unrelated history"}},
	}})
	sessionID.Store("session-b")
	runtime.GOMAXPROCS(previousProcs)

	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("extractor provider was not called")
	}
	// Queue B while A is blocked, then replace the live loop with C before A
	// finishes. A trailing run must consume the frozen B payload, never C.
	ext.OnLoopEnd(context.Background(), "end_turn")
	if !ext.Stats().Pending {
		t.Fatal("session B LoopEnd was not queued while A was in progress")
	}
	ext.mu.Lock()
	ext.lastFiredAt = time.Now().Add(-2 * AutoMemoryMinInterval)
	ext.mu.Unlock()
	loop.ResetSession([]llm.Message{{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "C_ONLY later history"}},
	}})
	sessionID.Store("session-c")
	close(provider.release)
	waitFor(t, 2*time.Second, func() bool {
		return ext.Stats().TotalExtractions == 2 && !ext.Stats().InProgress && ForkInflight() == 0
	})

	request := provider.firstRequest(t)
	var requestText strings.Builder
	for _, message := range request.Messages {
		for _, block := range message.Content {
			requestText.WriteString(block.Text)
		}
	}
	if got := requestText.String(); !strings.Contains(got, "A_ONLY") || strings.Contains(got, "B_ONLY") {
		t.Fatalf("extraction request crossed sessions: %q", got)
	}
	pendingRequest := provider.requestAt(t, 2)
	requestText.Reset()
	for _, message := range pendingRequest.Messages {
		for _, block := range message.Content {
			requestText.WriteString(block.Text)
		}
	}
	if got := requestText.String(); !strings.Contains(got, "B_ONLY") || strings.Contains(got, "C_ONLY") {
		t.Fatalf("pending extraction crossed sessions: %q", got)
	}

	raw, err := os.ReadFile(memoPath)
	if err != nil {
		t.Fatalf("read extracted memo: %v", err)
	}
	fm, body, err := memdir.ParseFile(raw)
	if err != nil {
		t.Fatalf("parse extracted memo: %v", err)
	}
	if !strings.Contains(string(body), "A_ONLY") || strings.Contains(string(body), "B_ONLY") {
		t.Fatalf("memo content crossed sessions: %q", body)
	}
	if fm.OriginSessionID != "session-a" || fm.SourceMessageID != "session-a/turn/1" {
		t.Fatalf("memo provenance crossed sessions: %+v", fm)
	}
	// A brand-new repository instance models the next Desktop/CLI process.
	// Auto Memory output must be part of the same retrieval corpus rather than
	// remaining visible only to the Dream subsystem that created it.
	restarted, err := memory.NewMemoryManager(root)
	if err != nil {
		t.Fatalf("restart repository: %v", err)
	}
	if recalled := restarted.PreviewAutoRetrieve("A_ONLY frozen extraction snapshot", 5); !strings.Contains(recalled, "A_ONLY") {
		t.Fatalf("fresh session did not recall Auto Memory topic: %q", recalled)
	}
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
		return err == nil && !ext.Stats().InProgress && ForkInflight() == 0
	})

	indexBytes, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("index not written: %v", err)
	}
	if !strings.Contains(string(indexBytes), "user_role.md") {
		t.Errorf("index missing memo: %q", indexBytes)
	}
}

func TestAutoMemoryExtractor_ExistingMemoEditIsDetectedAndFixedUp(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "feedback_language.md")
	original, err := memdir.RenderFile(&memdir.Frontmatter{
		Name: "language", Description: "reply language", Type: memdir.TypeFeedback,
	}, "Reply in Chinese.")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	pre, err := snapshotMemdirState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the filename and mtime stable: content hashing must still notice
	// this in-place Edit and route it through privacy + metadata fixup.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	modified, err := memdir.RenderFile(&memdir.Frontmatter{
		Name: "language", Description: "reply language", Type: memdir.TypeFeedback,
	}, "Reply in Chinese. OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz123456")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, modified, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	post, err := snapshotMemdirState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	touched := diffMemdirState(pre, post)
	if len(touched) != 1 || touched[0] != filepath.Base(path) {
		t.Fatalf("in-place edit not detected: %v", touched)
	}
	fixupTouchedMemosWithMetadata(root, touched, AutoMemorySource{
		SessionID: "session-a", MessageID: "message-b", Scope: "user", Confidence: 0.9,
	})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fm, body, err := memdir.ParseFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "sk-proj-") || !strings.Contains(string(body), "[REDACTED:") {
		t.Fatalf("privacy fixup did not redact existing memo: %s", body)
	}
	if fm.OriginSessionID != "session-a" || fm.SourceMessageID != "message-b" || fm.Scope != "user" {
		t.Fatalf("source metadata not refreshed: %+v", fm)
	}
	if fm.UpdatedAt == "" || fm.LastUsedAt == "" || fm.UseCount != 1 || fm.Confidence != 0.9 {
		t.Fatalf("retention metadata not refreshed: %+v", fm)
	}
	mode, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := mode.Mode().Perm(); got != 0o600 {
		t.Fatalf("memo mode = %o, want 600", got)
	}
}

func TestFixupTouchedMemos_RepairsUnframedMemoAndAlwaysRedacts(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project_release.md")
	if err := os.WriteFile(path, []byte(
		"Release coordination. OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz123456\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	fixupTouchedMemosWithMetadata(root, []string{filepath.Base(path)}, AutoMemorySource{
		SessionID: "session-a", Scope: "project",
	})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fm, body, err := memdir.ParseFile(raw)
	if err != nil {
		t.Fatalf("unframed memo was not repaired: %v\n%s", err, raw)
	}
	if err := fm.Validate(); err != nil {
		t.Fatalf("repaired metadata is invalid: %v (%+v)", err, fm)
	}
	if fm.Type != memdir.TypeProject || fm.Scope != "project" || fm.UpdatedAt == "" {
		t.Fatalf("repaired metadata = %+v", fm)
	}
	if strings.Contains(string(body), "sk-proj-") || !strings.Contains(string(body), "[REDACTED:") {
		t.Fatalf("privacy pass did not run on unframed memo: %s", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("repaired memo mode = %o, want 600", got)
	}
}

func TestAutoMemoryCurrentSourceDerivesSessionAndHistoryMessageID(t *testing.T) {
	loop := &Loop{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "first"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "answer"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "second"}}},
	}}
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: "session-derived"}
	}
	extractor := &AutoMemoryExtractor{loop: loop, memdirRoot: t.TempDir()}
	source := extractor.currentSource()
	if source.SessionID != "session-derived" || source.MessageID != "session-derived/turn/2" {
		t.Fatalf("derived source identity = %+v", source)
	}
	if source.Scope == "" || source.Confidence <= 0 {
		t.Fatalf("derived source defaults missing: %+v", source)
	}
}

func TestAutoMemoryCanonicalRootDefaultsToProjectScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	if _, err := memdir.DefaultRoot(); err != nil {
		t.Fatal(err)
	}
	source := AutoMemorySource{}
	applyAutoMemorySourceDefaults(&source)
	if source.Scope != "project" {
		t.Fatalf("canonical auto-memory scope = %q, want project so the repository can bind it to the active workspace", source.Scope)
	}
}

func TestAutoMemoryExtractor_InvalidationHookIsolatedAndPanicSafe(t *testing.T) {
	ext := &AutoMemoryExtractor{memdirRoot: "/tmp/memory"}
	var got MemoryInvalidation
	ext.SetInvalidationHook(func(change MemoryInvalidation) {
		got = change
		change.Changed[0] = "consumer-mutated.md"
	})
	changed := []string{"project_a.md"}
	ext.notifyInvalidation(changed)
	if got.Root != "/tmp/memory" || len(got.Changed) != 1 {
		t.Fatalf("unexpected invalidation: %+v", got)
	}
	if changed[0] != "project_a.md" {
		t.Fatalf("callback mutated extractor-owned slice: %v", changed)
	}
	// A bad consumer must not panic through dream finalization.
	ext.SetInvalidationHook(func(MemoryInvalidation) { panic("consumer bug") })
	ext.notifyInvalidation([]string{"project_b.md"})
}

func TestPrepareAutoMemoryPrefix_StripsSyntheticRecallWithoutMutatingParent(t *testing.T) {
	parent := []llm.Message{
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				{Type: "text", Text: "remember that I prefer concise replies"},
				{Type: "text", Text: "<auto-retrieve>old duplicated preference</auto-retrieve>", Synthetic: true},
			},
		},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "understood"}}},
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Type: "text",
				Text: "next question\n<auto-retrieve>legacy recalled project state</auto-retrieve>",
			}},
		},
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Type: "text", Text: "<auto-retrieve>synthetic-only recall</auto-retrieve>", Synthetic: true,
			}},
		},
	}

	got := prepareAutoMemoryPrefix(parent)
	if len(got) != 3 {
		t.Fatalf("filtered prefix length = %d, want 3: %+v", len(got), got)
	}
	var allText strings.Builder
	for _, message := range got {
		for _, block := range message.Content {
			allText.WriteString(block.Text)
		}
	}
	if text := allText.String(); strings.Contains(text, "auto-retrieve") ||
		strings.Contains(text, "duplicated preference") ||
		strings.Contains(text, "recalled project state") {
		t.Fatalf("synthetic recall leaked into extraction prefix: %q", text)
	}
	if got[2].Content[0].Text != "next question" {
		t.Fatalf("mixed legacy block was not reduced to visible text: %+v", got[2].Content)
	}
	if parent[0].Content[1].Text != "<auto-retrieve>old duplicated preference</auto-retrieve>" ||
		parent[2].Content[0].Text == "next question" {
		t.Fatal("filter mutated parent transcript")
	}
}

func TestBuildExtractorPrompt_ConsolidationGated(t *testing.T) {
	// No overlaps → no consolidation section (the common case).
	noOverlap := buildExtractorPrompt("/tmp/x", "m", []string{"a"}, nil)
	if strings.Contains(noOverlap, "Consolidation") {
		t.Error("consolidation section must be absent when no overlaps detected")
	}
	// Overlaps → section names the cluster + the archive_skill action.
	withOverlap := buildExtractorPrompt("/tmp/x", "m", []string{"a", "b"}, [][]string{{"git-commit", "commit-helper"}})
	if !strings.Contains(withOverlap, "Consolidation") ||
		!strings.Contains(withOverlap, "git-commit, commit-helper") ||
		!strings.Contains(withOverlap, "archive_skill") {
		t.Errorf("consolidation section missing/incomplete:\n%s", withOverlap)
	}
}

func TestBuildExtractorPrompt_IncludesManifest(t *testing.T) {
	got := buildExtractorPrompt("/tmp/x", "## existing\n- one\n", nil, nil)
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
func (p *blockingProvider) ModelID() string       { return "" }
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

type gateOrderingProvider struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once

	mu       sync.Mutex
	requests []llm.Request
}

func (p *gateOrderingProvider) Name() string          { return "gate-ordering" }
func (p *gateOrderingProvider) MaxContextTokens() int { return 200_000 }
func (p *gateOrderingProvider) ModelID() string       { return "test" }
func (p *gateOrderingProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	call := len(p.requests)
	p.mu.Unlock()
	if call == 1 {
		p.once.Do(func() { close(p.entered) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &llm.Response{
		Content:    []llm.ContentBlock{{Type: "text", Text: "no memory changes"}},
		StopReason: "end_turn",
	}, nil
}
func (p *gateOrderingProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, nil
}
func (p *gateOrderingProvider) requestsSnapshot() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}

func requestContainsText(req llm.Request, needle string) bool {
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if strings.Contains(block.Text, needle) {
				return true
			}
		}
	}
	return false
}

type sessionSwitchProvider struct {
	entered  chan struct{}
	release  chan struct{}
	memoPath string
	memo     string

	mu    sync.Mutex
	calls []llm.Request
}

func (p *sessionSwitchProvider) Name() string          { return "session-switch" }
func (p *sessionSwitchProvider) MaxContextTokens() int { return 200_000 }
func (p *sessionSwitchProvider) ModelID() string       { return "test" }
func (p *sessionSwitchProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	call := len(p.calls)
	p.mu.Unlock()
	if call == 1 {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &llm.Response{
			Content: []llm.ContentBlock{{
				Type: "tool_use", ToolUseID: "write-frozen", ToolName: "Write",
				ToolInput: map[string]any{"file_path": p.memoPath, "content": p.memo},
			}},
			StopReason: "tool_use",
		}, nil
	}
	return &llm.Response{
		Content:    []llm.ContentBlock{{Type: "text", Text: "saved frozen memory"}},
		StopReason: "end_turn",
	}, nil
}
func (p *sessionSwitchProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, nil
}
func (p *sessionSwitchProvider) firstRequest(t *testing.T) llm.Request {
	return p.requestAt(t, 0)
}
func (p *sessionSwitchProvider) requestAt(t *testing.T, index int) llm.Request {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.calls) {
		t.Fatalf("provider request %d unavailable; calls=%d", index, len(p.calls))
	}
	return p.calls[index]
}

type autoMemoryWriteTool struct{}

func (autoMemoryWriteTool) Name() string        { return "Write" }
func (autoMemoryWriteTool) Description() string { return "write an auto-memory test file" }
func (autoMemoryWriteTool) IsEnabled() bool     { return true }
func (autoMemoryWriteTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{"type": "string"},
			"content":   map[string]any{"type": "string"},
		},
		"required": []string{"file_path", "content"},
	}
}
func (autoMemoryWriteTool) Concurrency(map[string]any) pubtool.Concurrency {
	return pubtool.ConcurrencyExclusive
}
func (autoMemoryWriteTool) CanUse(context.Context, map[string]any) (pubtool.Permission, string) {
	return pubtool.PermissionAllow, ""
}
func (autoMemoryWriteTool) Execute(_ context.Context, input map[string]any) (*pubtool.Result, error) {
	path, _ := input["file_path"].(string)
	content, _ := input["content"].(string)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return nil, err
	}
	return &pubtool.Result{Output: "written"}, nil
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
