package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// orderRecorder is a goroutine-safe append-only string log used by
// dispatch tests that care about start order across the
// safe/queue/exclusive phases.
type orderRecorder struct {
	mu  sync.Mutex
	log []string
}

func (o *orderRecorder) Append(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.log = append(o.log, name)
}

func (o *orderRecorder) Snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, len(o.log))
	copy(out, o.log)
	return out
}

// fakeTool is a minimal tools.Tool for dispatch tests. Each instance
// records when it started/finished and reports its concurrency tier so
// the test can verify the dispatcher's phasing.
type fakeTool struct {
	name        string
	conc        tools.Concurrency
	starts      *atomic.Int64
	finishes    *atomic.Int64
	concurrent  *atomic.Int64
	maxConc     *atomic.Int64
	hold        time.Duration
	startsOrder *orderRecorder
}

func (f *fakeTool) Name() string                                 { return f.name }
func (f *fakeTool) Description() string                          { return "test" }
func (f *fakeTool) InputSchema() map[string]any                  { return map[string]any{"type": "object"} }
func (f *fakeTool) Concurrency(map[string]any) tools.Concurrency { return f.conc }
func (f *fakeTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (f *fakeTool) Execute(ctx context.Context, _ map[string]any) (*tools.Result, error) {
	if f.starts != nil {
		f.starts.Add(1)
	}
	if f.startsOrder != nil {
		f.startsOrder.Append(f.name)
	}
	if f.concurrent != nil {
		now := f.concurrent.Add(1)
		for {
			cur := f.maxConc.Load()
			if now <= cur || f.maxConc.CompareAndSwap(cur, now) {
				break
			}
		}
	}
	time.Sleep(f.hold)
	if f.concurrent != nil {
		f.concurrent.Add(-1)
	}
	if f.finishes != nil {
		f.finishes.Add(1)
	}
	return &tools.Result{Output: f.name}, nil
}

// TestDispatch_QueueSerializesAmongItself verifies that two queue-tier
// tools in the same batch run sequentially, never overlapping each
// other — even though the safe tools running alongside them DO fan out.
func TestDispatch_QueueSerializesAmongItself(t *testing.T) {
	queueConc := &atomic.Int64{}
	queueMax := &atomic.Int64{}

	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "Q1", conc: tools.ConcurrencyQueue,
		concurrent: queueConc, maxConc: queueMax,
		hold: 30 * time.Millisecond,
	})
	reg.Register(&fakeTool{
		name: "Q2", conc: tools.ConcurrencyQueue,
		concurrent: queueConc, maxConc: queueMax,
		hold: 30 * time.Millisecond,
	})

	loop := &Loop{Registry: reg}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "1", ToolName: "Q1"},
		{Type: "tool_use", ToolUseID: "2", ToolName: "Q2"},
	}
	out := make(chan Event, 32)
	if _, err := loop.executeBatch(context.Background(), uses, out, HookContext{}); err != nil {
		t.Fatalf("executeBatch err: %v", err)
	}
	if got := queueMax.Load(); got != 1 {
		t.Errorf("queue tools should be FIFO (max concurrent=1); got %d", got)
	}
}

// TestDispatch_QueueRunsAlongsideSafe verifies that queue and safe
// tools run concurrently — the queue tier doesn't block fanout, just
// serializes within itself.
func TestDispatch_QueueRunsAlongsideSafe(t *testing.T) {
	overall := &atomic.Int64{}
	overallMax := &atomic.Int64{}

	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "Safe1", conc: tools.ConcurrencySafe,
		concurrent: overall, maxConc: overallMax, hold: 30 * time.Millisecond,
	})
	reg.Register(&fakeTool{
		name: "Safe2", conc: tools.ConcurrencySafe,
		concurrent: overall, maxConc: overallMax, hold: 30 * time.Millisecond,
	})
	reg.Register(&fakeTool{
		name: "Q1", conc: tools.ConcurrencyQueue,
		concurrent: overall, maxConc: overallMax, hold: 30 * time.Millisecond,
	})

	loop := &Loop{Registry: reg}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "1", ToolName: "Safe1"},
		{Type: "tool_use", ToolUseID: "2", ToolName: "Safe2"},
		{Type: "tool_use", ToolUseID: "3", ToolName: "Q1"},
	}
	out := make(chan Event, 32)
	if _, err := loop.executeBatch(context.Background(), uses, out, HookContext{}); err != nil {
		t.Fatalf("executeBatch err: %v", err)
	}
	// At least 2 tools should have been concurrent (the two safe ones,
	// possibly with the queue running alongside).
	if got := overallMax.Load(); got < 2 {
		t.Errorf("safe tools should fan out (max concurrent>=2); got %d", got)
	}
}

// TestDispatch_ExclusiveRunsAfterAll verifies the existing exclusive
// tier still serializes AFTER the parallel + queue work — the new
// queue tier didn't perturb it.
func TestDispatch_ExclusiveRunsAfterAll(t *testing.T) {
	rec := &orderRecorder{}

	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "Safe1", conc: tools.ConcurrencySafe,
		startsOrder: rec, hold: 10 * time.Millisecond,
	})
	reg.Register(&fakeTool{
		name: "Q1", conc: tools.ConcurrencyQueue,
		startsOrder: rec, hold: 10 * time.Millisecond,
	})
	reg.Register(&fakeTool{
		name: "Excl1", conc: tools.ConcurrencyExclusive,
		startsOrder: rec, hold: 5 * time.Millisecond,
	})

	loop := &Loop{Registry: reg}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "1", ToolName: "Safe1"},
		{Type: "tool_use", ToolUseID: "2", ToolName: "Q1"},
		{Type: "tool_use", ToolUseID: "3", ToolName: "Excl1"},
	}
	out := make(chan Event, 32)
	if _, err := loop.executeBatch(context.Background(), uses, out, HookContext{}); err != nil {
		t.Fatalf("executeBatch err: %v", err)
	}
	order := rec.Snapshot()
	if len(order) != 3 {
		t.Fatalf("want 3 starts, got %d", len(order))
	}
	if order[len(order)-1] != "Excl1" {
		t.Errorf("Excl1 should start last; order=%v", order)
	}
}

// TestShortToolDesc — first paragraph wins, falls back to single-line,
// hard cap at 200 chars. Mirrors Crush's CRUSH_SHORT_TOOL_DESCRIPTIONS
// behaviour: a tool's full doc may be useful for `metis tools` listing
// but the LLM only needs enough to pick the tool.
func TestShortToolDesc(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"single line", "Read a file.", "Read a file."},
		{
			"paragraph break",
			"Read a file from disk.\n\nThis tool returns numbered lines.\n\nWhen the file is binary…",
			"Read a file from disk.",
		},
		{
			"single newline only",
			"Run a shell command.\nOutput is captured and truncated.",
			"Run a shell command.",
		},
		{
			"hard cap fallback",
			"" +
				"This is one giant runaway sentence with no breaks at all that just goes on " +
				"and on past two hundred characters so the boundary search finds neither a " +
				"paragraph nor a newline before the cap, forcing the truncate path to fire.",
			"This is one giant runaway sentence with no breaks at all that just goes on and on past two hundred characters so the boundary search finds neither a paragraph nor a newline before the cap, forcing the…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shortToolDesc(tc.in)
			if got != tc.want {
				t.Errorf("shortToolDesc(%q) =\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}

func (*fakeTool) IsEnabled() bool { return true }
