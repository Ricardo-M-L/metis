package mcp_tools

// lazy_server_test.go — locks the lazy-spawn behavior added in P7
// (kimi-cli `defer_mcp_tool_loading` parity). Coverage targets:
//
//	1. NewLazyServer doesn't spawn at construction
//	2. First Execute triggers spawn once; subsequent Executes reuse
//	3. spawnOnce serializes concurrent first-Execute callers
//	4. Spawn failure is sticky — no retry on next call
//	5. Close() on an unspawned lazy server is a no-op (no nil deref)
//	6. IsSpawned reflects state

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/mcp"
)

type countingCloseTransport struct{ closes atomic.Int32 }

func (t *countingCloseTransport) Close() error {
	t.closes.Add(1)
	return nil
}

// TestNewLazyServer_DoesNotSpawn — registering tools must not run the
// spawn closure. Verified by tracking call count and asserting it
// stays at zero through construction + Tools() + FilteredTools() +
// IsSpawned() — every read-only surface.
func TestNewLazyServer_DoesNotSpawn(t *testing.T) {
	var calls int32
	spawn := func(ctx context.Context) (*mcp.Client, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("test: spawn shouldn't run yet")
	}
	cached := []mcp.Tool{
		{Name: "screenshot", Description: "Take a screenshot", InputSchema: map[string]any{"type": "object"}},
	}
	srv := NewLazyServer("test-srv", cached, spawn)
	_ = srv.Tools()
	_ = srv.FilteredTools(nil, nil)
	if srv.IsSpawned() {
		t.Errorf("IsSpawned() should be false on a fresh lazy server")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("spawn closure called %d times before any Execute; want 0", got)
	}
}

// TestLazyServer_CloseNoOpWhenUnspawned — Close on a deferred server
// must not panic and must not invoke the spawn closure. Setup
// teardown in setupRuntime calls Close() on every registered server,
// so a nil-deref or accidental spawn-then-close would break shutdown.
func TestLazyServer_CloseNoOpWhenUnspawned(t *testing.T) {
	var calls int32
	spawn := func(context.Context) (*mcp.Client, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("test")
	}
	srv := NewLazyServer("test", nil, spawn)
	if err := srv.Close(); err != nil {
		t.Errorf("Close on unspawned lazy server should return nil; got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("Close should not invoke spawn; got %d calls", got)
	}
}

func TestLazyServerCloseCancelsAndRejectsInFlightSpawn(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	transport := &countingCloseTransport{}
	srv := NewLazyServer("closing", nil, func(context.Context) (*mcp.Client, error) {
		close(started)
		<-release // Simulate a launcher that returns after Close won the race.
		return mcp.NewClient(context.Background(), transport), nil
	})

	done := make(chan error, 1)
	go func() { done <- srv.ensureClient(context.Background()) }()
	<-started
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, errMCPServerClosed) {
		t.Fatalf("in-flight spawn after Close returned %v, want server closed", err)
	}
	if got := transport.closes.Load(); got != 1 {
		t.Fatalf("late client Close count = %d, want 1", got)
	}
	if srv.IsSpawned() {
		t.Fatal("late client was published after Server.Close")
	}
}

// TestServerClientSnapshotRejectsCloseBetweenEnsureAndUse deterministically
// models the shutdown interleaving that used to panic in Execute, GetPrompt,
// and ReadResource: ensureClient succeeds, Close clears s.client, then the
// operation snapshots the client. The snapshot must fail normally rather than
// return a nil client that the caller would dereference.
func TestServerClientSnapshotRejectsCloseBetweenEnsureAndUse(t *testing.T) {
	transport := &countingCloseTransport{}
	client := mcp.NewClient(context.Background(), transport)
	srv := &Server{client: client, name: "closing", tools: make(map[string]*MCPTool)}

	if err := srv.ensureClient(context.Background()); err != nil {
		t.Fatalf("ensure eager client: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("close server: %v", err)
	}
	got, err := srv.clientSnapshot()
	if got != nil || !errors.Is(err, errMCPServerClosed) {
		t.Fatalf("snapshot after Close = (%p, %v), want (nil, server closed)", got, err)
	}
	if transport.closes.Load() != 1 {
		t.Fatalf("transport Close count = %d, want 1", transport.closes.Load())
	}
}

// TestLazyServer_EnsureClient_SpawnsOnce — concurrent calls to
// ensureClient must collapse to a single spawn invocation. This is
// the core sync.Once contract; if it broke, parallel tool dispatch
// would spawn N subprocesses for N concurrent calls — exactly the
// resource waste lazy mode exists to prevent.
//
// Note: spawn returns an error (no real client to construct in a
// unit test), so ensureClient surfaces that error. We don't care
// about the return value — only about call count.
func TestLazyServer_EnsureClient_SpawnsOnce(t *testing.T) {
	var calls int32
	spawn := func(context.Context) (*mcp.Client, error) {
		atomic.AddInt32(&calls, 1)
		// Fail intentionally — building a real *mcp.Client in unit
		// scope is invasive, and the test only cares about call count.
		return nil, errors.New("intentional spawn fail for test")
	}
	srv := NewLazyServer("test", nil, spawn)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = srv.ensureClient(context.Background())
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("16 concurrent ensureClient calls → spawn ran %d times; want 1", got)
	}
}

// TestLazyServer_StickyFailure — once spawn fails, all subsequent
// callers see the same error without re-running spawn. Prevents a
// broken MCP binary from being hammered repeatedly.
func TestLazyServer_StickyFailure(t *testing.T) {
	wantErr := errors.New("binary not found")
	var calls int32
	spawn := func(context.Context) (*mcp.Client, error) {
		atomic.AddInt32(&calls, 1)
		return nil, wantErr
	}
	srv := NewLazyServer("test", nil, spawn)
	for i := 0; i < 5; i++ {
		got := srv.ensureClient(context.Background())
		if got == nil || got.Error() != wantErr.Error() {
			t.Errorf("call %d: got %v, want %v", i, got, wantErr)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("sticky failure broken: spawn ran %d times across 5 retries; want 1", got)
	}
}

// TestLazyServer_TruncatesDescription — the cached description still
// passes through the truncateDescription clamp at registration, so a
// 60 KB description in the cache file doesn't end up live in the
// system prompt.
func TestLazyServer_TruncatesDescription(t *testing.T) {
	bigDesc := make([]byte, maxToolDescriptionBytes+100)
	for i := range bigDesc {
		bigDesc[i] = 'x'
	}
	cached := []mcp.Tool{
		{Name: "tool", Description: string(bigDesc), InputSchema: map[string]any{"type": "object"}},
	}
	srv := NewLazyServer("test", cached, func(context.Context) (*mcp.Client, error) {
		return nil, errors.New("no spawn")
	})
	all := srv.Tools()
	if len(all) != 1 {
		t.Fatalf("expected 1 tool; got %d", len(all))
	}
	// MCPTool.Description() adds "[MCP] " prefix; the underlying
	// truncated string lives in t.description. Total returned should
	// be capped + ellipsis marker.
	desc := all[0].Description()
	if len(desc) > maxToolDescriptionBytes+200 { // 200-byte margin for prefix + ellipsis
		t.Errorf("oversize description not truncated; got %d bytes", len(desc))
	}
}

// TestLazyServer_NoSpawnFn — defensive: a Server constructed without
// either a client or a spawn closure (shouldn't happen, but) must
// fail-fast on Execute rather than nil-deref.
func TestLazyServer_NoSpawnFn(t *testing.T) {
	// NewLazyServer with nil spawn — caller bug, but we should
	// degrade gracefully.
	srv := NewLazyServer("orphan", nil, nil)
	err := srv.ensureClient(context.Background())
	if err == nil {
		t.Errorf("expected error from spawnless server; got nil")
	}
}
