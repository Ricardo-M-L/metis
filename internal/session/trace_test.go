package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTraceAppendAndEvents(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ev := &TraceEvent{SessionID: "s1", Kind: "text", Text: "hello world"}
	if err := store.Append(ev); err != nil {
		t.Fatal(err)
	}
	ev2 := &TraceEvent{SessionID: "s1", Kind: "tool_start", ToolName: "Bash"}
	if err := store.Append(ev2); err != nil {
		t.Fatal(err)
	}

	events := store.Events("s1")
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	if events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("sequence should be monotonic 1,2 got %d,%d", events[0].Sequence, events[1].Sequence)
	}
	if events[0].ID == "" || events[1].ID == "" {
		t.Fatal("event IDs must be auto-assigned")
	}
}

func TestTracePersistenceRoundtrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := store.Append(&TraceEvent{SessionID: "s9", Kind: "text", Text: "t" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen — events must reload from disk with intact sequences.
	store2, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	events := store2.Events("s9")
	if len(events) != 3 {
		t.Fatalf("want 3 events after reload, got %d", len(events))
	}
	if events[2].Sequence != 3 {
		t.Fatalf("want sequence 3, got %d", events[2].Sequence)
	}
}

func TestTraceSyncSessionFlushesBelowPeriodicThreshold(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Append(&TraceEvent{SessionID: "short", Kind: "loop_done", Text: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncSession("short"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "short.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"kind":"loop_done"`) {
		t.Fatalf("short trace was not flushed: %q", raw)
	}
}

func TestTraceSeparatesRepeatedProviderIDsByInvocation(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for _, ev := range []*TraceEvent{
		{SessionID: "repeat", Turn: 1, Kind: "tool_start", ToolName: "Agent", ToolUseID: "duplicate", TraceInvocationID: "inv-1", TraceCallID: "call-agent-1"},
		{SessionID: "repeat", Turn: 1, Kind: "text", Text: "first child", SubAgentOf: "duplicate", TraceInvocationID: "inv-1"},
		{SessionID: "repeat", Turn: 1, Kind: "tool_result", ToolUseID: "duplicate", ParentID: "duplicate", TraceInvocationID: "inv-1", TraceCallID: "call-agent-1"},
		{SessionID: "repeat", Turn: 1, Kind: "tool_start", ToolName: "Agent", ToolUseID: "duplicate", TraceInvocationID: "inv-2", TraceCallID: "call-agent-2"},
		{SessionID: "repeat", Turn: 1, Kind: "text", Text: "second child", SubAgentOf: "duplicate", TraceInvocationID: "inv-2"},
		{SessionID: "repeat", Turn: 1, Kind: "tool_result", ToolUseID: "duplicate", ParentID: "duplicate", TraceInvocationID: "inv-2", TraceCallID: "call-agent-2"},
	} {
		if err := store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	nodes := store.Trace("repeat")
	if len(nodes) != 6 {
		t.Fatalf("nodes = %+v", nodes)
	}
	wantText := []string{"", "first child", "", "", "second child", ""}
	wantDepth := []int{0, 1, 1, 0, 1, 1}
	for i := range nodes {
		if nodes[i].Event.Text != wantText[i] || nodes[i].Depth != wantDepth[i] {
			t.Fatalf("node %d = %+v, want text %q depth %d", i, nodes[i], wantText[i], wantDepth[i])
		}
	}
}

func TestTraceNestsInvocationOwnerUnderParentInvocation(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for _, ev := range []*TraceEvent{
		{SessionID: "nested", Turn: 1, Kind: "tool_start", ToolName: "Agent", ToolUseID: "parent", TraceInvocationID: "inv-parent"},
		{SessionID: "nested", Turn: 1, Kind: "tool_start", ToolName: "Fork", ToolUseID: "child", TraceInvocationID: "inv-child", TraceParentInvocationID: "inv-parent"},
		{SessionID: "nested", Turn: 1, Kind: "text", Text: "grandchild", SubAgentOf: "child", TraceInvocationID: "inv-child"},
	} {
		if err := store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	nodes := store.Trace("nested")
	if len(nodes) != 3 || nodes[0].Depth != 0 || nodes[1].Depth != 1 || nodes[2].Depth != 2 {
		t.Fatalf("nested nodes = %+v", nodes)
	}
}

func TestTraceDoesNotTreatOrdinaryToolAsInvocationOwner(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for _, ev := range []*TraceEvent{
		{SessionID: "ordinary", Turn: 1, Kind: "tool_start", ToolName: "Agent", ToolUseID: "agent", TraceInvocationID: "inv-agent", TraceCallID: "call-agent"},
		{SessionID: "ordinary", Turn: 1, Kind: "tool_start", ToolName: "Bash", ToolUseID: "bash", TraceInvocationID: "inv-agent", TraceCallID: "call-bash"},
		{SessionID: "ordinary", Turn: 1, Kind: "tool_result", ToolName: "Bash", ToolUseID: "bash", ParentID: "bash", TraceInvocationID: "inv-agent", TraceCallID: "call-bash"},
		{SessionID: "ordinary", Turn: 1, Kind: "text", Text: "child answer", SubAgentOf: "agent", TraceInvocationID: "inv-agent"},
		{SessionID: "ordinary", Turn: 1, Kind: "tool_result", ToolName: "Agent", ToolUseID: "agent", ParentID: "agent", TraceInvocationID: "inv-agent", TraceCallID: "call-agent"},
	} {
		if err := store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	nodes := store.Trace("ordinary")
	wantDepth := []int{0, 1, 2, 1, 1}
	if len(nodes) != len(wantDepth) {
		t.Fatalf("ordinary nodes = %+v", nodes)
	}
	for i, want := range wantDepth {
		if nodes[i].Depth != want {
			t.Fatalf("node %d = %+v, want depth %d", i, nodes[i], want)
		}
	}
}

func TestTraceSeparatesRepeatedOrdinaryToolIDsByCallAfterReload(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []*TraceEvent{
		{SessionID: "ordinary-repeat", Turn: 1, Kind: "tool_start", ToolName: "Agent", ToolUseID: "agent", TraceInvocationID: "inv-agent", TraceCallID: "call-agent"},
		{SessionID: "ordinary-repeat", Turn: 1, Kind: "tool_start", ToolName: "Bash", ToolUseID: "provider-reused", TraceInvocationID: "inv-agent", TraceCallID: "call-one"},
		{SessionID: "ordinary-repeat", Turn: 1, Kind: "tool_result", ToolName: "Bash", ToolUseID: "provider-reused", ParentID: "provider-reused", Text: "first", TraceInvocationID: "inv-agent", TraceCallID: "call-one"},
		{SessionID: "ordinary-repeat", Turn: 1, Kind: "tool_start", ToolName: "Bash", ToolUseID: "provider-reused", TraceInvocationID: "inv-agent", TraceCallID: "call-two"},
		{SessionID: "ordinary-repeat", Turn: 1, Kind: "tool_result", ToolName: "Bash", ToolUseID: "provider-reused", ParentID: "provider-reused", Text: "second", TraceInvocationID: "inv-agent", TraceCallID: "call-two"},
		{SessionID: "ordinary-repeat", Turn: 1, Kind: "tool_result", ToolName: "Agent", ToolUseID: "agent", ParentID: "agent", TraceInvocationID: "inv-agent", TraceCallID: "call-agent"},
	} {
		if err := store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	assertRepeatedOrdinaryTrace(t, store.Trace("ordinary-repeat"))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertRepeatedOrdinaryTrace(t, reopened.Trace("ordinary-repeat"))
}

func assertRepeatedOrdinaryTrace(t *testing.T, nodes []TracedNode) {
	t.Helper()
	wantKind := []string{"tool_start", "tool_start", "tool_result", "tool_start", "tool_result", "tool_result"}
	wantDepth := []int{0, 1, 2, 1, 2, 1}
	wantText := []string{"", "", "first", "", "second", ""}
	if len(nodes) != len(wantKind) {
		t.Fatalf("nodes = %+v", nodes)
	}
	for i := range nodes {
		if nodes[i].Event.Kind != wantKind[i] || nodes[i].Depth != wantDepth[i] || nodes[i].Event.Text != wantText[i] {
			t.Fatalf("node %d = %+v, want kind=%q depth=%d text=%q", i, nodes[i], wantKind[i], wantDepth[i], wantText[i])
		}
	}
}

func TestTraceSearch(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.Append(&TraceEvent{SessionID: "s1", Kind: "tool_result", Text: "compiled the auth module"})
	store.Append(&TraceEvent{SessionID: "s1", Kind: "tool_result", Text: "tests passed green"})
	store.Append(&TraceEvent{SessionID: "s2", Kind: "text", Text: "discuss auth with team"})

	// Single-term, cross-session.
	got, err := store.Search("auth", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 auth matches, got %d", len(got))
	}

	// AND semantics: term present in only one event.
	got, err = store.Search("compiled auth", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "compiled the auth module" {
		t.Fatalf("AND search wrong: %+v", got)
	}

	// Session-scoped.
	got, err = store.Search("team", "s2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("session-scoped search wrong: %d", len(got))
	}

	// No match anywhere.
	got, err = store.Search("zzznothere", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 matches, got %d", len(got))
	}

	// Empty query errors.
	if _, err := store.Search("   ", "", 0); err == nil {
		t.Fatal("empty query should error")
	}
}

func TestTraceTree(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Root events: a text burst, then a tool call whose result nests.
	store.Append(&TraceEvent{SessionID: "s1", Kind: "text", Text: "thinking"})
	store.Append(&TraceEvent{SessionID: "s1", Kind: "tool_start", ToolName: "Bash", ToolUseID: "t1"})
	store.Append(&TraceEvent{SessionID: "s1", Kind: "tool_result", ToolName: "Bash", ToolUseID: "t1", ParentID: "t1"})

	nodes := store.Trace("s1")
	if len(nodes) != 3 {
		t.Fatalf("want 3 tree nodes, got %d", len(nodes))
	}
	if nodes[0].Depth != 0 || nodes[0].Event.Kind != "text" {
		t.Fatalf("node0 wrong: depth=%d kind=%s", nodes[0].Depth, nodes[0].Event.Kind)
	}
	if nodes[1].Depth != 0 || nodes[1].Event.Kind != "tool_start" {
		t.Fatalf("node1 wrong: depth=%d kind=%s", nodes[1].Depth, nodes[1].Event.Kind)
	}
	if nodes[2].Depth != 1 || nodes[2].Event.Kind != "tool_result" {
		t.Fatalf("tool_result should nest at depth 1: depth=%d kind=%s", nodes[2].Depth, nodes[2].Event.Kind)
	}
}

func TestTraceTreeSeparatesRepeatedToolUseIDOccurrences(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Some providers restart their public tool IDs for every model response
	// (Gemini commonly emits gem_1 again). Legacy trace files do not carry a
	// TraceCallID, so the tree builder must still pair each result with the
	// corresponding occurrence instead of putting every result under the first
	// tool_start that used the raw provider ID.
	for _, ev := range []*TraceEvent{
		{SessionID: "repeated", Turn: 1, Kind: "tool_start", ToolName: "Bash", ToolUseID: "gem_1", Text: "first input"},
		{SessionID: "repeated", Turn: 1, Kind: "tool_result", ToolName: "Bash", ToolUseID: "gem_1", ParentID: "gem_1", Text: "first result"},
		{SessionID: "repeated", Turn: 1, Kind: "tool_start", ToolName: "Read", ToolUseID: "gem_1", Text: "second input"},
		{SessionID: "repeated", Turn: 1, Kind: "tool_result", ToolName: "Read", ToolUseID: "gem_1", ParentID: "gem_1", Text: "second result"},
	} {
		if err := store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	assertRepeatedLegacyOccurrences(t, store.Trace("repeated"))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening proves the fallback is computed from the stable legacy fields;
	// it does not rely on process-local state or rewrite the persisted JSONL.
	reopened, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertRepeatedLegacyOccurrences(t, reopened.Trace("repeated"))
}

func assertRepeatedLegacyOccurrences(t *testing.T, nodes []TracedNode) {
	t.Helper()
	wantKind := []string{"tool_start", "tool_result", "tool_start", "tool_result"}
	wantText := []string{"first input", "first result", "second input", "second result"}
	wantDepth := []int{0, 1, 0, 1}
	if len(nodes) != len(wantKind) {
		t.Fatalf("nodes = %+v", nodes)
	}
	for i := range nodes {
		if nodes[i].Event.Kind != wantKind[i] || nodes[i].Event.Text != wantText[i] || nodes[i].Depth != wantDepth[i] {
			t.Fatalf("node %d = %+v, want kind=%q text=%q depth=%d", i, nodes[i], wantKind[i], wantText[i], wantDepth[i])
		}
	}
}

func TestTraceTreeRepeatedToolUseIDCannotCrossTurns(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// A partial first turn must not claim an ownerless result from a later
	// turn just because the provider reused the same public ID.
	for _, ev := range []*TraceEvent{
		{SessionID: "cross-turn", Turn: 1, Kind: "tool_start", ToolName: "Bash", ToolUseID: "gem_1"},
		{SessionID: "cross-turn", Turn: 2, Kind: "tool_result", ToolName: "Read", ToolUseID: "gem_1", ParentID: "gem_1", Text: "orphaned later result"},
	} {
		if err := store.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	nodes := store.Trace("cross-turn")
	if len(nodes) != 2 || nodes[0].Depth != 0 || nodes[1].Depth != 0 {
		t.Fatalf("cross-turn nodes = %+v", nodes)
	}
}

func TestTraceTreeArgsDeltaCannotStealToolResult(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.Append(&TraceEvent{SessionID: "s1", Kind: "tool_args", ToolName: "Bash", ToolUseID: "t1", Text: `{"command":"`})
	store.Append(&TraceEvent{SessionID: "s1", Kind: "tool_args", ToolName: "Bash", ToolUseID: "t1", Text: `echo hi"}`})
	store.Append(&TraceEvent{SessionID: "s1", Kind: "tool_start", ToolName: "Bash", ToolUseID: "t1", Text: `{"command":"echo hi"}`})
	store.Append(&TraceEvent{SessionID: "s1", Kind: "tool_result", ToolName: "Bash", ToolUseID: "t1", ParentID: "t1", Text: "ok"})

	nodes := store.Trace("s1")
	if len(nodes) != 4 {
		t.Fatalf("want 4 nodes, got %d", len(nodes))
	}
	if nodes[2].Event.Kind != "tool_start" || nodes[2].Depth != 0 {
		t.Fatalf("tool start = %+v", nodes[2])
	}
	if nodes[3].Event.Kind != "tool_result" || nodes[3].Depth != 1 {
		t.Fatalf("tool result was stolen by args delta: %+v", nodes[3])
	}
}

func TestTraceNextTurn(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if n := store.NextTurn("s1"); n != 1 {
		t.Fatalf("first turn want 1, got %d", n)
	}
	store.Append(&TraceEvent{SessionID: "s1", Kind: "text", Text: "x"})
	if n := store.NextTurn("s1"); n != 2 {
		t.Fatalf("second turn want 2, got %d", n)
	}
	store.Append(&TraceEvent{SessionID: "s1", Kind: "text", Text: "y"})
	turns := store.Events("s1")
	if turns[0].Turn != 1 || turns[1].Turn != 2 {
		t.Fatalf("turns misassigned: %d %d", turns[0].Turn, turns[1].Turn)
	}
}

func TestTraceCorruptFileIgnored(t *testing.T) {
	dir := t.TempDir()
	// Write a valid event then a corrupt line; load must not fail.
	store, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.Append(&TraceEvent{SessionID: "s1", Kind: "text", Text: "ok"})
	store.Close()
	if err := writeCorruptTraceLine(dir); err != nil {
		t.Fatal(err)
	}

	store2, err := NewTraceStore(dir)
	if err != nil {
		t.Fatalf("corrupt trace line must not break load: %v", err)
	}
	defer store2.Close()
	if len(store2.Events("s1")) != 1 {
		t.Fatal("valid events should survive a corrupt trailing line")
	}
}

func TestTraceTreeSubagentNesting(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Spawn tree: text → tool_start (subagent) → subagent_end → text (from subagent)
	store.Append(&TraceEvent{SessionID: "s1", Kind: "text", Text: "thinking"})
	store.Append(&TraceEvent{SessionID: "s1", Kind: "subagent_start", ToolUseID: "sa1"})
	store.Append(&TraceEvent{SessionID: "s1", Kind: "subagent_end", ToolUseID: "sa1", SubAgentOf: "sa1"})
	store.Append(&TraceEvent{SessionID: "s1", Kind: "text", Text: "subagent work", SubAgentOf: "sa1"})

	nodes := store.Trace("s1")
	// Expect: text(depth 0) → subagent_start(depth 0) → subagent_end(depth 1) → text(depth 1)
	if len(nodes) != 4 {
		t.Fatalf("want 4 nodes, got %d", len(nodes))
	}
	if nodes[0].Depth != 0 || nodes[0].Event.Kind != "text" {
		t.Fatalf("node0: depth=%d kind=%s", nodes[0].Depth, nodes[0].Event.Kind)
	}
	if nodes[1].Depth != 0 || nodes[1].Event.Kind != "subagent_start" {
		t.Fatalf("node1: depth=%d kind=%s", nodes[1].Depth, nodes[1].Event.Kind)
	}
	if nodes[2].Depth != 1 || nodes[2].Event.Kind != "subagent_end" {
		t.Fatalf("node2: depth=%d kind=%s (want 1/subagent_end)", nodes[2].Depth, nodes[2].Event.Kind)
	}
	if nodes[3].Depth != 1 || nodes[3].Event.Text != "subagent work" {
		t.Fatalf("node3: depth=%d text=%q", nodes[3].Depth, nodes[3].Event.Text)
	}
}

func TestTraceAppendFailureDoesNotAdvanceSequence(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Normal append — sequence advances.
	store.Append(&TraceEvent{SessionID: "s1", Kind: "text", Text: "ok"})
	events := store.Events("s1")
	if len(events) != 1 || events[0].Sequence != 1 {
		t.Fatalf("first event: len=%d seq=%d", len(events), events[0].Sequence)
	}
}

func TestTruncateTraceRune(t *testing.T) {
	cjk := "编译错误在鉴权模块" // 12 CJK chars = 36 bytes at UTF-8
	got := TruncateTrace(cjk, 5)
	if got != "编译错误在...(truncated)" {
		t.Fatalf("rune truncation wrong: got %q", got)
	}
}

func writeCorruptTraceLine(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, "s1.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write([]byte("{ not valid json\n"))
	return err
}

func TestTraceCJKSearch(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.Append(&TraceEvent{SessionID: "s1", Kind: "tool_result", Text: "编译错误：鉴权模块未通过"})
	store.Append(&TraceEvent{SessionID: "s1", Kind: "text", Text: "测试全部通过"})

	// Contiguous Chinese substring must match (unigram+bigram tokens).
	got, err := store.Search("编译错误", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "编译错误：鉴权模块未通过" {
		t.Fatalf("CJK search wrong: %+v", got)
	}

	// Mixed CJK + ASCII terms still AND.
	got, err = store.Search("鉴权 auth", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("mixed AND should match nothing: %+v", got)
	}
	store.Append(&TraceEvent{SessionID: "s1", Kind: "text", Text: "auth 鉴权修复完成"})
	got, err = store.Search("鉴权 auth", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "auth 鉴权修复完成" {
		t.Fatalf("mixed AND should match the combined event: %+v", got)
	}

	// Single CJK char query works via unigrams.
	got, err = store.Search("译", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("single CJK char search wrong: %+v", got)
	}
}

func TestTraceLazyLoad(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.Append(&TraceEvent{SessionID: "s1", Kind: "text", Text: "one"})
	store.Append(&TraceEvent{SessionID: "s2", Kind: "text", Text: "two"})
	store.Close()

	// Fresh store must not read anything until a session is touched.
	store2, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	store2.mu.RLock()
	loaded := len(store2.loaded)
	store2.mu.RUnlock()
	if loaded != 0 {
		t.Fatalf("NewTraceStore must be lazy, found %d preloaded sessions", loaded)
	}

	// Touching one session loads only that session.
	if evs := store2.Events("s1"); len(evs) != 1 {
		t.Fatalf("s1 should have 1 event, got %d", len(evs))
	}
	store2.mu.RLock()
	loaded = len(store2.loaded)
	store2.mu.RUnlock()
	if loaded != 1 {
		t.Fatalf("only s1 should be loaded, got %d", loaded)
	}

	// Appending to a not-yet-loaded session resumes its sequence.
	if err := store2.Append(&TraceEvent{SessionID: "s2", Kind: "text", Text: "more"}); err != nil {
		t.Fatal(err)
	}
	evs := store2.Events("s2")
	if len(evs) != 2 || evs[1].Sequence != 2 {
		t.Fatalf("lazy append should resume sequence: %+v", evs)
	}
}

func TestTraceInvalidSessionID(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, sid := range []string{"", "../escape", "a/b", `a\b`, "..", ".", "line\nbreak", "nul\x00byte"} {
		if err := store.Append(&TraceEvent{SessionID: sid, Kind: "text"}); err == nil {
			t.Fatalf("session id %q must be rejected", sid)
		}
	}
	if _, err := store.Search("x", "../escape", 0); err == nil {
		t.Fatal("search with traversal session id must be rejected")
	}
}

func TestTraceDeleteRemovesDiskAndMemoryState(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if turn := store.NextTurn("target"); turn != 1 {
		t.Fatalf("target turn = %d; want 1", turn)
	}
	targetEvent := &TraceEvent{SessionID: "target", Kind: "text", Text: "shared target-only"}
	otherEvent := &TraceEvent{SessionID: "other", Kind: "text", Text: "shared other-only"}
	if err := store.Append(targetEvent); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(otherEvent); err != nil {
		t.Fatal(err)
	}
	targetWriter := store.writers["target"]
	if targetWriter == nil || store.writers["other"] == nil {
		t.Fatal("test requires both session writers to be open")
	}

	if err := store.Delete("target"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "target.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("target trace still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.jsonl")); err != nil {
		t.Fatalf("other trace was removed: %v", err)
	}
	if _, err := targetWriter.f.Write([]byte("must fail")); err == nil {
		t.Fatal("target writer remained open after delete")
	}

	store.mu.RLock()
	_, hasWriter := store.writers["target"]
	_, hasLoaded := store.loaded["target"]
	_, hasSequence := store.seq["target"]
	_, hasEvents := store.events["target"]
	_, hasTurn := store.turned["target"]
	_, hasEventID := store.ids[targetEvent.ID]
	otherWriter := store.writers["other"]
	indexedTarget := false
	for _, bySession := range store.index {
		if _, ok := bySession["target"]; ok {
			indexedTarget = true
			break
		}
	}
	store.mu.RUnlock()
	if hasWriter || hasLoaded || hasSequence || hasEvents || hasTurn || hasEventID || indexedTarget {
		t.Fatalf("target state remains: writer=%v loaded=%v seq=%v events=%v turn=%v id=%v index=%v",
			hasWriter, hasLoaded, hasSequence, hasEvents, hasTurn, hasEventID, indexedTarget)
	}
	if otherWriter == nil {
		t.Fatal("other session writer was affected")
	}

	got, err := store.Search("shared", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "other" {
		t.Fatalf("search retained deleted session or lost other session: %+v", got)
	}
}

func TestTraceDeleteIsIdempotentAndResetsSequence(t *testing.T) {
	store, err := NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.Append(&TraceEvent{SessionID: "target", Kind: "text", Text: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("target"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("target"); err != nil {
		t.Fatalf("second delete must be idempotent: %v", err)
	}

	ev := &TraceEvent{SessionID: "target", Kind: "text", Text: "new"}
	if err := store.Append(ev); err != nil {
		t.Fatal(err)
	}
	if ev.Sequence != 1 || ev.Turn != 0 {
		t.Fatalf("state was not reset: sequence=%d turn=%d", ev.Sequence, ev.Turn)
	}
}

func TestTraceDeleteRejectsUnsafeSessionIDs(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sentinel := filepath.Join(dir, "sentinel.jsonl")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"", ".", "..", "../sentinel", "a/b", `a\b`, "line\nbreak", "nul\x00byte"} {
		if err := store.Delete(sid); err == nil {
			t.Errorf("Delete(%q) succeeded; want validation error", sid)
		}
	}
	if body, err := os.ReadFile(sentinel); err != nil || string(body) != "keep" {
		t.Fatalf("unsafe delete altered sentinel: body=%q err=%v", body, err)
	}
}
