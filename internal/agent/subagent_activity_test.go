package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func newActivityFixture(t *testing.T) (string, string, *SubAgentTranscript, *SubAgentActivityRecorder) {
	t.Helper()
	dir, id := t.TempDir(), "agt-activity"
	transcript, err := NewSubAgentTranscript(dir, id, NewSubAgentHeader(id, "synthetic-model", "parent-activity", "fixture", "", "default"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	recorder, err := NewSubAgentActivityRecorder(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	return dir, id, transcript, recorder
}

func activityStart(trace, provider, name string, input map[string]any) Event {
	return Event{Kind: EventToolStart, TraceCallID: trace, ToolUseID: provider, ToolName: name, ToolInput: input}
}

func activityResult(trace, provider, name, output string, isError bool) Event {
	return Event{Kind: EventToolResult, TraceCallID: trace, ToolUseID: provider, ToolName: name, ToolResult: &ToolResult{Output: output, IsError: isError}}
}

func observeActivity(t *testing.T, recorder *SubAgentActivityRecorder, events ...Event) {
	t.Helper()
	for _, event := range events {
		if err := recorder.Observe(event); err != nil {
			t.Fatal(err)
		}
	}
}

func loadActivity(t *testing.T, dir, id string) SubAgentActivitySnapshot {
	t.Helper()
	snapshot, err := LoadSubAgentActivity(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestSubAgentActivityLiveBeforeCheckpointKeepsFullValues(t *testing.T) {
	t.Parallel()
	dir, id, _, recorder := newActivityFixture(t)
	argument := "ARG-BEGIN\n" + strings.Repeat("完整输入𐐀\n", 5000) + "ARG-END"
	output := "OUT-BEGIN\n" + strings.Repeat("完整输出 <value>\n", 10000) + "OUT-END"
	input := map[string]any{"command": argument, "nested": map[string]any{"items": []any{"original", true}}}
	event := activityStart("trace-full", "provider-full", "Bash", input)
	event.TraceInvocationID = "invocation-real"
	observeActivity(t, recorder, event)
	input["command"] = "mutated later"
	input["nested"].(map[string]any)["items"].([]any)[0] = "mutated later"
	live := loadActivity(t, dir, id)
	if !live.Available || live.Partial || len(live.Calls) != 1 || live.Calls[0].Status != "running" || live.Calls[0].Input["command"] != argument {
		t.Fatalf("wrong live state: available=%v partial=%v calls=%d", live.Available, live.Partial, len(live.Calls))
	}
	if got := live.Calls[0].Input["nested"].(map[string]any)["items"].([]any)[0]; got != "original" {
		t.Fatalf("input was not detached: %v", got)
	}
	conversation, err := LoadSubAgentConversation(dir, id)
	if err != nil || len(conversation.Messages) != 0 {
		t.Fatalf("fixture must prove tool is visible before any history checkpoint: messages=%d err=%v", len(conversation.Messages), err)
	}
	observeActivity(t, recorder, activityResult("trace-full", "provider-full", "Bash", output, true))
	completed := loadActivity(t, dir, id)
	call := completed.Calls[0]
	if call.Result != output || call.Input["command"] != argument || !call.IsError || call.Status != "error" || call.TraceCallID != "trace-full" || call.TraceInvocationID != "invocation-real" || call.StartedAt.IsZero() || call.EndedAt == nil {
		t.Fatal("full arguments, result, error state, or actual trace identity changed")
	}
	if call.ID != live.Calls[0].ID || completed.Revision == live.Revision {
		t.Fatal("occurrence identity must stay stable while revision changes")
	}
	if revision := SubAgentActivityRevision(dir, id); revision != completed.Revision {
		t.Fatalf("cheap revision=%q differs from full snapshot=%q", revision, completed.Revision)
	}
}

func TestSubAgentActivityRepeatedProviderIDsKeepOccurrenceAndOrder(t *testing.T) {
	t.Parallel()
	dir, id, _, recorder := newActivityFixture(t)
	observeActivity(t, recorder,
		activityStart("trace-first", "reused", "Read", map[string]any{"path": "first"}),
		activityStart("trace-second", "reused", "Read", map[string]any{"path": "second"}),
		activityResult("trace-second", "reused", "Read", "second completed first", false),
		activityResult("trace-first", "reused", "Read", "first completed second", false),
		activityStart("trace-third", "reused", "Read", map[string]any{"path": "third"}),
		activityResult("trace-third", "reused", "Read", "third", true),
	)
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	got := loadActivity(t, dir, id)
	if got.Partial || len(got.Calls) != 3 {
		t.Fatalf("occurrences lost: partial=%v count=%d", got.Partial, len(got.Calls))
	}
	seen := map[string]bool{}
	for i, want := range []string{"first completed second", "second completed first", "third"} {
		call := got.Calls[i]
		if call.Result != want || call.ID == "" || seen[call.ID] || call.ToolUseID != "reused" {
			t.Fatalf("wrong occurrence %d: %#v", i, call)
		}
		seen[call.ID] = true
	}
}

func TestSubAgentActivityAmbiguityAndOrphanNeverInventCompletion(t *testing.T) {
	t.Parallel()
	dir, id, _, recorder := newActivityFixture(t)
	observeActivity(t, recorder,
		activityStart("", "duplicate", "Read", nil),
		activityStart("", "duplicate", "Read", nil),
		activityResult("", "duplicate", "Read", "ambiguous actual result", false),
		activityResult("unobserved", "duplicate", "Read", "another actual result", true),
	)
	live := loadActivity(t, dir, id)
	if !live.Partial || live.Reason != "unmatched_tool_result" || len(live.Calls) != 4 {
		t.Fatalf("ambiguity was hidden: %#v", live)
	}
	if live.Calls[0].Status != "running" || live.Calls[1].Status != "running" || live.Calls[2].Status != "result_only" || live.Calls[3].Status != "result_only" {
		t.Fatal("orphan results completed unrelated starts")
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	closed := loadActivity(t, dir, id)
	if closed.Calls[0].Status != "incomplete" || closed.Calls[1].Status != "incomplete" || closed.Calls[0].EndedAt == nil || closed.Calls[2].Result != "ambiguous actual result" {
		t.Fatal("close must preserve real results and mark unfinished calls incomplete")
	}
}

func TestSubAgentActivityLegacyPairsOnlyUniquePending(t *testing.T) {
	t.Parallel()
	dir, id, _, recorder := newActivityFixture(t)
	observeActivity(t, recorder,
		activityStart("", "legacy", "Read", nil),
		activityResult("", "legacy", "Read", "first", false),
		activityStart("", "legacy", "Read", nil),
		activityResult("", "legacy", "Read", "second", false),
	)
	got := loadActivity(t, dir, id)
	if got.Partial || len(got.Calls) != 2 || got.Calls[0].Result != "first" || got.Calls[1].Result != "second" || got.Calls[0].ID == got.Calls[1].ID {
		t.Fatal("legacy sequential reuse was merged or mispaired")
	}
}

func TestSubAgentActivityReplacementRunRejectsOldPendingAndLateWrites(t *testing.T) {
	t.Parallel()
	dir, id, _, old := newActivityFixture(t)
	observeActivity(t, old, activityStart("old-trace", "reused", "Read", nil))
	fresh, err := NewSubAgentActivityRecorder(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	observeActivity(t, fresh, activityStart("new-trace", "reused", "Read", nil))
	observeActivity(t, old, activityResult("old-trace", "reused", "Read", "must stay in old inode", false))
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	got := loadActivity(t, dir, id)
	if len(got.Calls) != 1 || got.Calls[0].TraceCallID != "new-trace" || got.Calls[0].Status != "running" || got.Calls[0].Result != "" {
		t.Fatal("late writes from an earlier run leaked into the replacement")
	}
	// Even a result with the old provider ID cannot complete a new occurrence.
	observeActivity(t, fresh, activityResult("old-trace", "reused", "Read", "orphan in new run", false))
	got = loadActivity(t, dir, id)
	if len(got.Calls) != 2 || got.Calls[0].Status != "running" || got.Calls[1].Status != "result_only" || !got.Partial {
		t.Fatal("cross-run result was guessed onto current pending call")
	}
}

func TestSubAgentActivityHeaderReplacementCannotLeakAcrossParents(t *testing.T) {
	t.Parallel()
	dir, id, transcript, recorder := newActivityFixture(t)
	observeActivity(t, recorder, activityStart("old", "provider", "Read", map[string]any{"path": "private-parent-A"}))
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := NewSubAgentTranscript(dir, id, NewSubAgentHeader(id, "synthetic-model", "parent-B", "fixture", "", "default"))
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	got := loadActivity(t, dir, id)
	if got.Available || !got.Partial || got.Reason != "owner_mismatch" || got.ParentSessionID != "parent-B" || len(got.Calls) != 0 {
		t.Fatalf("old parent contents leaked: %#v", got)
	}
}

func TestSubAgentActivitySameParentNewHeaderRejectsStaleRun(t *testing.T) {
	t.Parallel()
	dir, id, transcript, recorder := newActivityFixture(t)
	observeActivity(t, recorder, activityStart("old", "provider", "Read", map[string]any{"path": "old-run"}))
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	header, err := LoadSubAgentHeader(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	header.Model = "replacement-synthetic-model"
	replacement, err := NewSubAgentTranscript(dir, id, header)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	got := loadActivity(t, dir, id)
	if got.Available || !got.Partial || got.Reason != "owner_mismatch" || len(got.Calls) != 0 {
		t.Fatal("matching parent alone incorrectly admitted an old transcript generation")
	}
}

func TestSubAgentActivityIgnoresCrashTailWithoutMutatingFile(t *testing.T) {
	t.Parallel()
	dir, id, _, recorder := newActivityFixture(t)
	observeActivity(t, recorder, activityStart("trace", "provider", "Read", nil))
	path, _ := subAgentActivityPath(dir, id)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"kind":"result","call":{"output":"crash`)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	got := loadActivity(t, dir, id)
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || !got.Available || !got.Partial || got.Reason != "incomplete_tail" || len(got.Calls) != 1 || got.Calls[0].Status != "running" || got.Calls[0].Result != "" {
		t.Fatal("crash tail mutated history or invented a completed result")
	}
}

func TestSubAgentActivityNilContextShadowAndForwardedChildren(t *testing.T) {
	t.Parallel()
	dir, id, _, recorder := newActivityFixture(t)
	parent := WithSubAgentActivityRecorder(context.Background(), recorder)
	child := WithSubAgentActivityRecorder(parent, nil)
	event := activityStart("trace", "provider", "Read", nil)
	if err := ObserveSubAgentActivity(child, event); err != nil {
		t.Fatal(err)
	}
	if err := CloseSubAgentActivity(child); err != nil {
		t.Fatal(err)
	}
	forwarded := event
	forwarded.SubAgentParentID = "grandchild-parent-tool"
	observeActivity(t, recorder, forwarded, Event{Kind: EventTextDelta, TextDelta: "not a tool boundary"})
	out := make(chan Event, 1)
	emit(parent, out, event)
	event = <-out
	if err := ObserveSubAgentActivity(parent, event); err != nil {
		t.Fatal(err)
	}
	if err := CloseSubAgentActivity(parent); err != nil {
		t.Fatal(err)
	}
	if err := ObserveSubAgentActivity(nil, event); err != nil {
		t.Fatal(err)
	}
	if err := CloseSubAgentActivity(nil); err != nil {
		t.Fatal(err)
	}
	got := loadActivity(t, dir, id)
	if len(got.Calls) != 1 || !got.Partial || got.Calls[0].Status != "incomplete" {
		t.Fatal("nil shadow, native child scope, or runner close was not respected")
	}
}

func TestSubAgentActivityWriteFailureIsAdvisoryAndVisible(t *testing.T) {
	t.Parallel()
	dir, id, _, recorder := newActivityFixture(t)
	observeActivity(t, recorder, activityStart("trace", "provider", "Read", nil))
	// A simulated storage failure does not run a tool or modify its Event value.
	if err := recorder.f.Close(); err != nil {
		t.Fatal(err)
	}
	event := activityResult("trace", "provider", "Read", "real provider output remains intact", true)
	before := event
	if err := recorder.Observe(event); err == nil {
		t.Fatal("expected a write failure")
	}
	if !reflect.DeepEqual(event, before) {
		t.Fatal("advisory recording changed the actual runtime event")
	}
	got := loadActivity(t, dir, id)
	if !got.Available || !got.Partial || got.Reason != "write_error" || got.Calls[0].Status != "running" {
		t.Fatal("storage failure silently presented completion")
	}
	if SubAgentActivityRevision(dir, id) != got.Revision {
		t.Fatal("failed-writer revision is inconsistent")
	}
}

func TestSubAgentActivityFailedResumeSetupHidesPreviousRun(t *testing.T) {
	t.Parallel()
	dir, id, _, recorder := newActivityFixture(t)
	observeActivity(t, recorder, activityStart("old", "provider", "Read", map[string]any{"path": "old-run"}))
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	path, _ := subAgentActivityPath(dir, id)
	// A non-empty directory at the publication path deterministically makes
	// atomic rename fail without relying on root-sensitive permission checks.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "block"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSubAgentActivityRecorder(dir, id); err == nil {
		t.Fatal("expected recorder publication to fail")
	}
	got := loadActivity(t, dir, id)
	if got.Available || !got.Partial || got.Reason != "initialization_error" || len(got.Calls) != 0 {
		t.Fatal("failed new setup exposed a preceding run")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".activity-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("failed setup leaked temporary files: %v err=%v", matches, err)
	}
}

func TestSubAgentActivityRejectsMalformedCompleteRecordsAndInvalidPaths(t *testing.T) {
	t.Parallel()
	dir, id, _, recorder := newActivityFixture(t)
	for _, invalid := range []string{"", "..", "../escaped", "a/b", `a\b`, "\x00bad"} {
		if _, err := NewSubAgentActivityRecorder(dir, invalid); err == nil {
			t.Fatalf("writer accepted invalid ID %q", invalid)
		}
		if _, err := LoadSubAgentActivity(dir, invalid); err == nil {
			t.Fatalf("reader accepted invalid ID %q", invalid)
		}
	}
	path, _ := subAgentActivityPath(dir, id)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{broken-complete-json}\n")
	_ = f.Close()
	if _, err := LoadSubAgentActivity(dir, id); err == nil {
		t.Fatal("malformed complete record was silently hidden")
	}
	_ = recorder.Close()
}

func TestSubAgentActivityRefusesSymlinkSidecarAndTranscript(t *testing.T) {
	t.Parallel()
	dir, id, transcript, recorder := newActivityFixture(t)
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	path, _ := subAgentActivityPath(dir, id)
	target := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSubAgentActivity(dir, id); err == nil {
		t.Fatal("reader followed a sidecar symlink")
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	transcriptPath := transcript.Path()
	transcriptTarget := filepath.Join(t.TempDir(), "outside-transcript.jsonl")
	if err := os.Rename(transcriptPath, transcriptTarget); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(transcriptTarget, transcriptPath); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSubAgentActivityRecorder(dir, id); err == nil {
		t.Fatal("writer followed a transcript symlink")
	}
}

func TestSubAgentActivityMissingAndBoundedFilesAreExplicit(t *testing.T) {
	t.Parallel()
	dir, id, _, recorder := newActivityFixture(t)
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	path, _ := subAgentActivityPath(dir, id)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got := loadActivity(t, dir, id)
	if got.Available || got.Reason != "missing" || got.Calls == nil {
		t.Fatal("old runtime must have explicit unavailable activity")
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(subAgentActivityMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	got = loadActivity(t, dir, id)
	if got.Available || !got.Partial || got.Reason != "size_limit" {
		t.Fatal("oversized activity must be explicit instead of truncated")
	}
}

func TestSubAgentActivityConcurrentReadDuringNativeWrites(t *testing.T) {
	t.Parallel()
	dir, id, _, recorder := newActivityFixture(t)
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; i < 40; i++ {
			trace := string(rune('A' + i))
			if err := recorder.Observe(activityStart(trace, "same-provider-id", "Read", nil)); err != nil {
				t.Error(err)
				return
			}
			if err := recorder.Observe(activityResult(trace, "same-provider-id", "Read", strings.Repeat(trace, 4096), false)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 40; i++ {
		got := loadActivity(t, dir, id)
		for _, call := range got.Calls {
			if call.Status == "completed" && (call.EndedAt == nil || call.Result == "") {
				t.Fatal("concurrent snapshot invented completion")
			}
		}
	}
	writers.Wait()
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	got := loadActivity(t, dir, id)
	if got.Partial || len(got.Calls) != 40 {
		t.Fatalf("final snapshot lost boundaries: count=%d partial=%v", len(got.Calls), got.Partial)
	}
}

func TestSubAgentActivityRejectsTamperedResultPair(t *testing.T) {
	t.Parallel()
	dir, id, _, recorder := newActivityFixture(t)
	observeActivity(t, recorder, activityStart("trace", "provider", "Read", nil), activityResult("trace", "provider", "Read", "actual", false))
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	path, _ := subAgentActivityPath(dir, id)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(data, []byte{'\n'})
	var result subAgentActivityRecord
	if err := json.Unmarshal(lines[2], &result); err != nil {
		t.Fatal(err)
	}
	result.Call.ToolUseID = "different-provider"
	lines[2], err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Join(lines, []byte{'\n'}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSubAgentActivity(dir, id); err == nil {
		t.Fatal("reader accepted a fabricated result pairing")
	}
}

// Drive the real stream -> dispatch -> Loop.emit chain. Native child events
// already carry SubAgentParentID, and Agent/Fork/Ralph tool events carry the
// next invocation in TraceInvocationID plus this loop in TraceParentInvocationID.
func TestSubAgentActivityRealLoopEmitOwnsNativeToolsAndRejectsOtherLoops(t *testing.T) {
	dir, id, _, recorder := newActivityFixture(t)
	ownerCtx := WithParentToolUseID(context.Background(), "reused-parent-provider-id")
	ownerCtx = WithTraceInvocationID(ownerCtx, "owner-loop-invocation")
	ownerCtx = WithSubAgentActivityRecorder(ownerCtx, recorder)
	run := func(ctx context.Context, names ...string) []Event {
		t.Helper()
		registry := tools.NewRegistry()
		calls := make([]llm.ContentBlock, 0, len(names))
		for _, name := range names {
			registry.Register(planBatchTestTool{name: name})
			calls = append(calls, llm.ContentBlock{Type: "tool_use", ToolUseID: "reused-tool-provider-id", ToolName: name})
		}
		provider := &planModeStreamProvider{streams: [][]llm.StreamEvent{toolBatchEvents(calls...)}}
		loop := NewLoop(provider, registry, permission.New(permission.ModeBypass), nil, "synthetic system", 4)
		loop.AppendUser("synthetic tools only")
		out := make(chan Event, 256)
		if err := loop.Run(ctx, out); err != nil {
			t.Fatalf("real Loop.Run: %v", err)
		}
		close(out)
		var events []Event
		for event := range out {
			events = append(events, event)
		}
		return events
	}
	var grandchildInvocation string
	starts := 0
	for _, event := range run(ownerCtx, "Read", "Agent", "Fork", "Ralph") {
		if event.Kind == EventToolStart {
			starts++
			if event.SubAgentParentID != "reused-parent-provider-id" {
				t.Fatal("regression did not exercise native child parent stamping")
			}
			if event.ToolName == "Agent" {
				if event.TraceInvocationID == "owner-loop-invocation" || event.TraceParentInvocationID != "owner-loop-invocation" {
					t.Fatal("regression did not exercise native child Agent invocation ownership")
				}
				grandchildInvocation = event.TraceInvocationID
			}
		}
		if err := ObserveSubAgentActivity(ownerCtx, event); err != nil {
			t.Fatal(err)
		}
	}
	got := loadActivity(t, dir, id)
	if starts != 4 || len(got.Calls) != 4 || got.Partial {
		t.Fatalf("native child tools were lost after Loop.emit: starts=%d saved=%d partial=%v", starts, len(got.Calls), got.Partial)
	}
	seen := map[string]bool{}
	for _, call := range got.Calls {
		if call.Status != "completed" || call.Result != call.ToolName+" ok" || call.TraceCallID == "" {
			t.Fatalf("native call was not paired: %#v", call)
		}
		seen[call.ToolName] = true
	}
	for _, name := range []string{"Read", "Agent", "Fork", "Ralph"} {
		if !seen[name] {
			t.Fatalf("missing native %s tool", name)
		}
	}
	// Use real loops with the same public parent ID and provider tool IDs. A
	// grandchild or sibling must still fail the immutable invocation check.
	for _, invocation := range []string{grandchildInvocation, "sibling-loop-invocation"} {
		foreignCtx := WithParentToolUseID(context.Background(), "reused-parent-provider-id")
		foreignCtx = WithTraceInvocationID(foreignCtx, invocation)
		for _, event := range run(foreignCtx, "Read", "Agent", "Fork") {
			if err := ObserveSubAgentActivity(ownerCtx, event); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Invocation equality alone is insufficient: the public parent binding
	// must also agree, even if a context accidentally reuses a trace identity.
	wrongParent := WithParentToolUseID(context.Background(), "another-parent-provider-id")
	wrongParent = WithTraceInvocationID(wrongParent, "owner-loop-invocation")
	for _, event := range run(wrongParent, "Read", "Agent") {
		if err := ObserveSubAgentActivity(ownerCtx, event); err != nil {
			t.Fatal(err)
		}
	}
	if got := loadActivity(t, dir, id); len(got.Calls) != 4 || got.Partial {
		t.Fatalf("foreign loop tools leaked: calls=%d partial=%v", len(got.Calls), got.Partial)
	}
	// Reusing a recorder in a different context cannot move its owner.
	foreignCtx := WithParentToolUseID(context.Background(), "another-parent-provider-id")
	foreignCtx = WithTraceInvocationID(foreignCtx, "sibling-loop-invocation")
	foreignCtx = WithSubAgentActivityRecorder(foreignCtx, recorder)
	for _, event := range run(foreignCtx, "Read") {
		if err := ObserveSubAgentActivity(foreignCtx, event); err != nil {
			t.Fatal(err)
		}
	}
	if got := loadActivity(t, dir, id); len(got.Calls) != 4 {
		t.Fatal("recorder owner was rebound to another loop")
	}
	// Direct Agent.Execute callers can lack dispatch's invocation context. The
	// wrapper returns a context whose real Loop.emit still has a unique owner.
	fallbackDir, fallbackID, _, fallback := newActivityFixture(t)
	fallbackCtx := WithParentToolUseID(context.Background(), "direct-parent-provider-id")
	fallbackCtx = WithSubAgentActivityRecorder(fallbackCtx, fallback)
	if TraceInvocationIDFromContext(fallbackCtx) == "" {
		t.Fatal("direct embedding child lacks an immutable loop invocation")
	}
	for _, event := range run(fallbackCtx, "Read", "Agent", "Fork") {
		if err := ObserveSubAgentActivity(fallbackCtx, event); err != nil {
			t.Fatal(err)
		}
	}
	if got := loadActivity(t, fallbackDir, fallbackID); len(got.Calls) != 3 || got.Partial {
		t.Fatalf("direct embedding native calls were lost: count=%d partial=%v", len(got.Calls), got.Partial)
	}
}
