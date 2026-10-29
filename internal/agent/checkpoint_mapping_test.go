package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func durableRewindLoop(t *testing.T, id, work, shadow string, history []llm.Message) *Loop {
	t.Helper()
	// BeginRun also owns settings lock files; keep them out of the real user
	// profile even though snapshots already use an explicit temporary root.
	fixtureHome := filepath.Join(shadow, ".fixture-home")
	if err := os.MkdirAll(fixtureHome, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fixtureHome)
	t.Setenv("METIS_HOME", filepath.Join(shadow, ".fixture-metis"))
	l := &Loop{ckptSnappedAt: -1}
	l.Restore(history)
	l.SetCheckpointer(checkpoint.NewManager(id, work, shadow))
	return l
}
func durableRewindEdit(t *testing.T, l *Loop, call, path, text string) {
	t.Helper()
	input := map[string]any{"path": path}
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: call, TraceCallID: call, ToolName: "Edit", ToolInput: input}}})
	ctx := context.WithValue(context.Background(), rewindCallContextKey{}, call)
	if err := l.snapPreEditContext(ctx, "Edit", input); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if err := l.recordCheckpointMutationContext(ctx, "Edit", input, &tools.Result{Output: "ok"}, nil); err != nil {
		t.Fatal(err)
	}
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: call, TraceCallID: call, ToolResult: "ok"}}}, msg(llm.RoleAssistant, "done"))
}
func TestDurableRewindAcrossIndependentLoops(t *testing.T) {
	work, shadow := t.TempDir(), t.TempDir()
	path := filepath.Join(work, "code.txt")
	os.WriteFile(path, []byte("v1"), 0600)
	writer := durableRewindLoop(t, "durable", work, shadow, nil)
	writer.AppendUser("edit file")
	durableRewindEdit(t, writer, "edit-1", path, "v2")
	reader := durableRewindLoop(t, "durable", work, shadow, writer.History())
	if err := reader.LoadRewindState(); err != nil {
		t.Fatal(err)
	}
	points := reader.RewindPoints()
	if len(points) != 1 || !points[0].HasCodeCheckpoint {
		t.Fatalf("durable point unavailable: %+v", points)
	}
	if result, err := reader.RewindToTurn(1, RewindCode); err != nil || !result.CodeRestored {
		t.Fatalf("code restore=%+v err=%v", result, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "v1" {
		t.Fatalf("restored=%q", data)
	}
	again := durableRewindLoop(t, "durable", work, shadow, reader.History())
	if _, err := again.RewindToTurn(1, RewindCode); err != nil {
		t.Fatalf("updated post-restore fingerprints were not durable: %v", err)
	}
}
func TestDurableRewindPendingWriteIsNotACompleteSnapshot(t *testing.T) {
	work, shadow := t.TempDir(), t.TempDir()
	path := filepath.Join(work, "code.txt")
	os.WriteFile(path, []byte("v1"), 0600)
	writer := durableRewindLoop(t, "killed", work, shadow, nil)
	writer.AppendUser("edit file")
	input := map[string]any{"path": path}
	writer.Messages = append(writer.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "killed-write", TraceCallID: "killed-write", ToolName: "Edit", ToolInput: input}}})
	ctx := context.WithValue(context.Background(), rewindCallContextKey{}, "killed-write")
	if err := writer.snapPreEditContext(ctx, "Edit", input); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("already changed before SIGKILL"), 0600)
	reader := durableRewindLoop(t, "killed", work, shadow, writer.History())
	if _, err := reader.RewindToTurn(1, RewindCode); !errors.Is(err, ErrCheckpointIncomplete) {
		t.Fatalf("pending call authorized code restore: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "already changed before SIGKILL" {
		t.Fatal("pending restore changed the file")
	}
	if result, err := reader.RewindToTurn(1, RewindConversation); err != nil || !result.ConversationRestored {
		t.Fatalf("conversation unavailable: %+v %v", result, err)
	}
}
func TestDurableRewindFailedToolPartialWriteBlocksCode(t *testing.T) {
	work, shadow := t.TempDir(), t.TempDir()
	path := filepath.Join(work, "code.txt")
	os.WriteFile(path, []byte("v1"), 0600)
	writer := durableRewindLoop(t, "failed", work, shadow, nil)
	writer.AppendUser("bash writes then fails")
	input := map[string]any{"command": "printf partial > code.txt; exit 1"}
	ctx := context.WithValue(context.Background(), rewindCallContextKey{}, "failed-bash")
	writer.Messages = append(writer.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "failed-bash", TraceCallID: "failed-bash", ToolName: "Bash", ToolInput: input}}})
	if err := writer.snapPreEditContext(ctx, "Bash", input); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("partial"), 0600)
	if err := writer.recordCheckpointMutationContext(ctx, "Bash", input, &tools.Result{IsError: true, Output: "exit 1"}, nil); !errors.Is(err, ErrCheckpointIncomplete) {
		t.Fatalf("failure=%v", err)
	}
	writer.Messages = append(writer.Messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "failed-bash", TraceCallID: "failed-bash", IsError: true, ToolResult: "exit 1"}}})
	reader := durableRewindLoop(t, "failed", work, shadow, writer.History())
	if points := reader.RewindPoints(); len(points) != 1 || points[0].HasCodeCheckpoint || points[0].CodeUnavailableReason == "" {
		t.Fatalf("failed mutation misrepresented: %+v", points)
	}
}
func TestDurableRewindOldJournalCannotCoverNewUnrecordedMutation(t *testing.T) {
	work, shadow := t.TempDir(), t.TempDir()
	path := filepath.Join(work, "code.txt")
	os.WriteFile(path, []byte("v1"), 0600)
	writer := durableRewindLoop(t, "tail", work, shadow, nil)
	writer.AppendUser("first edit")
	durableRewindEdit(t, writer, "first-edit", path, "v2")
	old, _ := os.ReadFile(writer.Checkpointer.RewindMetadataPath())
	writer.AppendUser("later edit whose journal is lost")
	durableRewindEdit(t, writer, "lost-edit", path, "v3")
	os.WriteFile(writer.Checkpointer.RewindMetadataPath(), old, 0600)
	reader := durableRewindLoop(t, "tail", work, shadow, writer.History())
	for _, point := range reader.RewindPoints() {
		if point.HasCodeCheckpoint {
			t.Fatalf("old mapping covered an unknown mutation: %+v", point)
		}
	}
	if _, err := reader.RewindToTurn(1, RewindCode); !errors.Is(err, ErrCheckpointIncomplete) {
		t.Fatalf("unknown tail restore: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "v3" {
		t.Fatal("unknown tail changed file")
	}
}
func TestDurableRewindRejectsStalePrefixAndTampering(t *testing.T) {
	for _, variant := range []string{"prefix", "session", "corrupt", "symlink"} {
		t.Run(variant, func(t *testing.T) {
			work, shadow := t.TempDir(), t.TempDir()
			path := filepath.Join(work, "code.txt")
			os.WriteFile(path, []byte("v1"), 0600)
			writer := durableRewindLoop(t, "bound", work, shadow, nil)
			writer.AppendUser("original prompt")
			durableRewindEdit(t, writer, "bound-edit", path, "v2")
			history := writer.History()
			id := "bound"
			metadata := writer.Checkpointer.RewindMetadataPath()
			switch variant {
			case "prefix":
				history[0] = msg(llm.RoleUser, "different prompt")
			case "session":
				id = "other"
				target := checkpoint.NewManager(id, work, shadow).RewindMetadataPath()
				os.MkdirAll(filepath.Dir(target), 0700)
				data, _ := os.ReadFile(metadata)
				os.WriteFile(target, data, 0600)
			case "corrupt":
				os.WriteFile(metadata, []byte("{"), 0600)
			case "symlink":
				outside := filepath.Join(t.TempDir(), "metadata.json")
				data, _ := os.ReadFile(metadata)
				os.WriteFile(outside, data, 0600)
				os.Remove(metadata)
				os.Symlink(outside, metadata)
			}
			reader := durableRewindLoop(t, id, work, shadow, history)
			if err := reader.LoadRewindState(); err == nil {
				t.Fatal("invalid metadata was accepted")
			}
			for _, point := range reader.RewindPoints() {
				if point.HasCodeCheckpoint {
					t.Fatal("invalid metadata enabled code")
				}
			}
			if _, err := reader.RewindToTurn(1, RewindCode); err == nil {
				t.Fatal("invalid metadata restored code")
			}
			if data, _ := os.ReadFile(path); string(data) != "v2" {
				t.Fatal("invalid metadata modified files")
			}
		})
	}
}
func TestDurableRewindConversationOnlyKeepsDiscardedFileEffectsBlocked(t *testing.T) {
	work, shadow := t.TempDir(), t.TempDir()
	a, b := filepath.Join(work, "a"), filepath.Join(work, "b")
	os.WriteFile(a, []byte("a0"), 0600)
	writer := durableRewindLoop(t, "conversation", work, shadow, nil)
	writer.AppendUser("edit a")
	durableRewindEdit(t, writer, "edit-a", a, "a1")
	writer.AppendUser("create b")
	durableRewindEdit(t, writer, "create-b", b, "b1")
	if _, err := writer.RewindToTurn(2, RewindConversation); err != nil {
		t.Fatal(err)
	}
	reader := durableRewindLoop(t, "conversation", work, shadow, writer.History())
	if _, err := reader.RewindToTurn(1, RewindCode); !errors.Is(err, ErrCheckpointIncomplete) {
		t.Fatalf("discarded file B was falsely covered by snapshot A: %v", err)
	}
	if data, _ := os.ReadFile(b); string(data) != "b1" {
		t.Fatal("conversation rewind changed B")
	}
}

func TestDurableRewindDispatchRefusesWhenFenceCannotBeSaved(t *testing.T) {
	work, shadow := t.TempDir(), t.TempDir()
	path := filepath.Join(work, "code.txt")
	os.WriteFile(path, []byte("v1"), 0600)
	writer := durableRewindLoop(t, "unwritable", work, shadow, nil)
	writer.AppendUser("first edit")
	durableRewindEdit(t, writer, "first", path, "v2")
	metadata := writer.Checkpointer.RewindMetadataPath()
	os.Remove(metadata)
	os.Mkdir(metadata, 0700)
	writer.AppendUser("this command must not execute")
	called := false
	tool := checkpointCancelTool{run: func() { called = true; os.WriteFile(path, []byte("unprotected change"), 0600) }}
	result := writer.runExecute(context.Background(), tool, llm.ContentBlock{Type: "tool_use", ToolName: "Bash", ToolUseID: "blocked", TraceCallID: "blocked", ToolInput: map[string]any{"command": "write"}}, make(chan Event, 64), HookContext{}, 0, "", "", "blocked")
	if called || !result.IsError || !strings.Contains(result.ToolResult, "本次修改未执行") {
		t.Fatalf("unwritable fence allowed dispatch: called=%v result=%+v", called, result)
	}
	if data, _ := os.ReadFile(path); string(data) != "v2" {
		t.Fatal("refused tool changed code")
	}
}
func TestDurableRewindParallelCallPendingIsNotClearedByAnotherCall(t *testing.T) {
	work, shadow := t.TempDir(), t.TempDir()
	a, b := filepath.Join(work, "a"), filepath.Join(work, "b")
	os.WriteFile(a, []byte("a0"), 0600)
	os.WriteFile(b, []byte("b0"), 0600)
	writer := durableRewindLoop(t, "parallel", work, shadow, nil)
	writer.AppendUser("edit both")
	inputA, inputB := map[string]any{"path": a}, map[string]any{"path": b}
	writer.Messages = append(writer.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolName: "Edit", ToolUseID: "a", TraceCallID: "a", ToolInput: inputA}, {Type: "tool_use", ToolName: "Edit", ToolUseID: "b", TraceCallID: "b", ToolInput: inputB}}})
	ctxA := context.WithValue(context.Background(), rewindCallContextKey{}, "a")
	ctxB := context.WithValue(context.Background(), rewindCallContextKey{}, "b")
	if err := writer.snapPreEditContext(ctxA, "Edit", inputA); err != nil {
		t.Fatal(err)
	}
	if err := writer.snapPreEditContext(ctxB, "Edit", inputB); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(a, []byte("a1"), 0600)
	os.WriteFile(b, []byte("b1"), 0600)
	if err := writer.recordCheckpointMutationContext(ctxA, "Edit", inputA, nil, nil); err != nil {
		t.Fatal(err)
	}
	reader := durableRewindLoop(t, "parallel", work, shadow, writer.History())
	if reader.RewindPoints()[0].HasCodeCheckpoint {
		t.Fatal("first completed call erased another pending call")
	}
	if err := writer.recordCheckpointMutationContext(ctxB, "Edit", inputB, nil, nil); err != nil {
		t.Fatal(err)
	}
	reader = durableRewindLoop(t, "parallel", work, shadow, writer.History())
	if !reader.RewindPoints()[0].HasCodeCheckpoint {
		t.Fatalf("both completed calls unavailable: %+v", reader.RewindPoints())
	}
}
func TestDurableRewindCorruptCodeMetadataStillAllowsConversation(t *testing.T) {
	work, shadow := t.TempDir(), t.TempDir()
	path := filepath.Join(work, "code.txt")
	os.WriteFile(path, []byte("v1"), 0600)
	writer := durableRewindLoop(t, "corrupt-conversation", work, shadow, nil)
	writer.AppendUser("edit file")
	durableRewindEdit(t, writer, "edit", path, "v2")
	os.WriteFile(writer.Checkpointer.RewindMetadataPath(), []byte("{"), 0600)
	reader := durableRewindLoop(t, "corrupt-conversation", work, shadow, writer.History())
	if reader.LoadRewindState() == nil {
		t.Fatal("corrupt code metadata accepted")
	}
	result, err := reader.RewindToTurn(1, RewindConversation)
	if err != nil || !result.ConversationRestored || reader.CountTurns() != 0 {
		t.Fatalf("conversation rejected after successful persistence: %+v %v", result, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "v2" {
		t.Fatal("conversation restore changed code")
	}
}
func TestDurableRewindDelegatedMutationRemainsUnattributed(t *testing.T) {
	for _, name := range []string{"Agent", "Task", "Fork", "MessageTeammate"} {
		t.Run(name, func(t *testing.T) {
			work, shadow := t.TempDir(), t.TempDir()
			a, b := filepath.Join(work, "a"), filepath.Join(work, "child-created")
			os.WriteFile(a, []byte("a0"), 0600)
			writer := durableRewindLoop(t, "delegation", work, shadow, nil)
			writer.AppendUser("edit a")
			durableRewindEdit(t, writer, "edit-a", a, "a1")
			writer.AppendUser("delegate")
			input := map[string]any{"prompt": "child may change files"}
			ctx := context.WithValue(context.Background(), rewindCallContextKey{}, "delegate")
			writer.Messages = append(writer.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolName: name, ToolUseID: "delegate", TraceCallID: "delegate", ToolInput: input}}})
			if err := writer.snapPreEditContext(ctx, name, input); err == nil {
				t.Fatal("unattributed delegate was presented as protected")
			}
			os.WriteFile(b, []byte("child result"), 0600)
			_ = writer.recordCheckpointMutationContext(ctx, name, input, &tools.Result{Output: "done"}, nil)
			writer.Messages = append(writer.Messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "delegate", TraceCallID: "delegate", ToolResult: "done"}}})
			reader := durableRewindLoop(t, "delegation", work, shadow, writer.History())
			if _, err := reader.RewindToTurn(1, RewindCode); !errors.Is(err, ErrCheckpointUnavailable) {
				t.Fatalf("delegate side effects falsely covered: %v", err)
			}
			if _, err := reader.RewindToTurn(2, RewindConversation); err != nil {
				t.Fatal(err)
			}
			if data, _ := os.ReadFile(b); string(data) != "child result" {
				t.Fatal("conversation restore changed child output")
			}
		})
	}
}
func TestDurableRewindCapacityWritesSmallFailureFence(t *testing.T) {
	work, shadow := t.TempDir(), t.TempDir()
	writer := durableRewindLoop(t, "capacity", work, shadow, nil)
	writer.AppendUser("capacity")
	for i := 0; i < 4097; i++ {
		writer.ckptStack = append(writer.ckptStack, ckptEntry{restoreToTurns: 0, label: "point"})
	}
	if err := writer.persistRewindState(); err == nil {
		t.Fatal("capacity should disable code recovery")
	}
	data, err := os.ReadFile(writer.Checkpointer.RewindMetadataPath())
	if err != nil || len(data) > 4096 {
		t.Fatalf("small failure fence missing: size=%d err=%v", len(data), err)
	}
	if writer.ckptWriteErr != nil {
		t.Fatalf("capacity was treated as unwritable storage: %v", writer.ckptWriteErr)
	}
	reader := durableRewindLoop(t, "capacity", work, shadow, writer.History())
	if reader.LoadRewindState() == nil {
		t.Fatal("capacity tombstone advertised code availability")
	}
}

func TestDurableRewindConcurrentPostHooksCopyStablePathMaps(t *testing.T) {
	work, shadow := t.TempDir(), t.TempDir()
	a, b := filepath.Join(work, "a"), filepath.Join(work, "b")
	os.WriteFile(a, []byte("a0"), 0600)
	os.WriteFile(b, []byte("b0"), 0600)
	writer := durableRewindLoop(t, "concurrent-post", work, shadow, nil)
	writer.AppendUser("edit concurrently")
	inputA, inputB := map[string]any{"path": a}, map[string]any{"path": b}
	ctxA := context.WithValue(context.Background(), rewindCallContextKey{}, "a")
	ctxB := context.WithValue(context.Background(), rewindCallContextKey{}, "b")
	if err := writer.snapPreEditContext(ctxA, "Edit", inputA); err != nil {
		t.Fatal(err)
	}
	if err := writer.snapPreEditContext(ctxB, "Edit", inputB); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(a, []byte("a1"), 0600)
	os.WriteFile(b, []byte("b1"), 0600)
	done := make(chan error, 2)
	go func() { done <- writer.recordCheckpointMutationContext(ctxA, "Edit", inputA, nil, nil) }()
	go func() { done <- writer.recordCheckpointMutationContext(ctxB, "Edit", inputB, nil, nil) }()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	reader := durableRewindLoop(t, "concurrent-post", work, shadow, writer.History())
	if _, err := reader.RewindToTurn(1, RewindCode); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(a); string(data) != "a0" {
		t.Fatalf("a=%q", data)
	}
	if data, _ := os.ReadFile(b); string(data) != "b0" {
		t.Fatalf("b=%q", data)
	}
}

func TestDurableRewindDelegationCannotBeBypassedByLaterBaseline(t *testing.T) {
	work, shadow := t.TempDir(), t.TempDir()
	a := filepath.Join(work, "a")
	os.WriteFile(a, []byte("a0"), 0600)
	writer := durableRewindLoop(t, "sticky-delegation", work, shadow, nil)
	writer.AppendUser("delegate work")
	input := map[string]any{"run_in_background": true}
	ctx := context.WithValue(context.Background(), rewindCallContextKey{}, "background")
	_ = writer.snapPreEditContext(ctx, "Agent", input)
	writer.Messages = append(writer.Messages, msg(llm.RoleAssistant, "child started"))
	writer.AppendUser("own later edit")
	// The parent can continue useful work, but no new local baseline proves that
	// the background child has stopped modifying other files.
	durableRewindEdit(t, writer, "later-parent-edit", a, "a1")
	reader := durableRewindLoop(t, "sticky-delegation", work, shadow, writer.History())
	for _, point := range reader.RewindPoints() {
		if point.HasCodeCheckpoint {
			t.Fatalf("later baseline bypassed unknown child: %+v", point)
		}
	}
	if _, err := reader.RewindToTurn(2, RewindCode); err == nil {
		t.Fatal("background-child uncertainty lost on restart")
	}
	if _, err := reader.RewindToTurn(2, RewindConversation); err != nil {
		t.Fatal(err)
	}
}

func TestDurableRewindMetadataRejectsSessionSymlinkBeforeCreating(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	session := filepath.Join(root, "session")
	if err := os.Symlink(outside, session); err != nil {
		t.Fatal(err)
	}
	if err := writeRewindState(filepath.Join(session, ".git", "rewind.json"), []byte("{}")); err == nil {
		t.Fatal("symlink session was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, ".git")); !os.IsNotExist(err) {
		t.Fatalf("metadata writer created outside storage before validation: %v", err)
	}
}

func TestDurableRewindResumeSameUserTurnKeepsOriginalBaseline(t *testing.T) {
	work, shadow := t.TempDir(), t.TempDir()
	path := filepath.Join(work, "code.txt")
	os.WriteFile(path, []byte("v1"), 0600)
	writer := durableRewindLoop(t, "same-user-turn", work, shadow, nil)
	writer.AppendUser("two edits before completion")
	durableRewindEdit(t, writer, "first-edit", path, "v2")
	resumed := durableRewindLoop(t, "same-user-turn", work, shadow, writer.History())
	durableRewindEdit(t, resumed, "resumed-edit", path, "v3")
	if len(resumed.ckptStack) != 1 {
		t.Fatalf("resumed a single user turn with %d baselines, want one original baseline", len(resumed.ckptStack))
	}
	if _, err := resumed.RewindToTurn(1, RewindCode); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "v1" {
		t.Fatalf("same turn lost original preimage: %q", data)
	}
}
