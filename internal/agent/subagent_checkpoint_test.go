package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func newCheckpointTranscript(t *testing.T) (string, *SubAgentTranscript) {
	t.Helper()
	dir := t.TempDir()
	tr, err := NewSubAgentTranscript(dir, "agt-checkpoint", NewSubAgentHeader("agt-checkpoint", "m", "parent", "alice", "", "bypass"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return dir, tr
}

func checkpointMessage(text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: text}}}
}

func TestSubAgentCheckpointSuffixResetAndResume(t *testing.T) {
	dir, tr := newCheckpointTranscript(t)
	if err := tr.BindHandoff("workspace", "alice"); err != nil {
		t.Fatal(err)
	}
	history := []llm.Message{checkpointMessage("first"), checkpointMessage("second")}
	if err := tr.Checkpoint(history); err != nil {
		t.Fatal(err)
	}
	history = append(history, checkpointMessage("third"))
	if err := tr.Checkpoint(history); err != nil {
		t.Fatal(err)
	}
	// Same length with changed content also requires reset, not just a cursor.
	history[1] = checkpointMessage("edited second")
	if err := tr.Checkpoint(history); err != nil {
		t.Fatal(err)
	}
	history = []llm.Message{checkpointMessage("compacted summary")}
	if err := tr.Checkpoint(history); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadSubAgentSnapshot(dir, "agt-checkpoint")
	if err != nil || !reflect.DeepEqual(snap.Messages, history) {
		t.Fatalf("snapshot=%#v err=%v", snap, err)
	}
	before, err := os.ReadFile(tr.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(before), `"reset":true`) != 2 {
		t.Fatalf("expected edits and compaction reset records: %s", before)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := ResumeSubAgentTranscript(dir, "agt-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if err := resumed.BindHandoff("workspace", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := resumed.BindHandoff("other", "alice"); err == nil {
		t.Fatal("changed mailbox accepted")
	}
	if err := resumed.Checkpoint(history); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := os.ReadFile(resumed.Path())
	if string(unchanged) != string(before) {
		t.Fatal("unchanged resumed history duplicated records")
	}
	history = append(history, checkpointMessage("follow-up"))
	if err := resumed.Checkpoint(history); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(resumed.Path())
	if !strings.HasPrefix(string(after), string(before)) {
		t.Fatal("resume rewrote prior acknowledged bytes")
	}
	snap, err = LoadSubAgentSnapshot(dir, "agt-checkpoint")
	if err != nil || !reflect.DeepEqual(snap.Messages, history) {
		t.Fatalf("resumed=%#v err=%v", snap, err)
	}
}

func TestSubAgentCheckpointCrashTailPreservesLastSyncedHistory(t *testing.T) {
	dir, tr := newCheckpointTranscript(t)
	history := []llm.Message{checkpointMessage("saved envelope")}
	if err := tr.Checkpoint(history); err != nil {
		t.Fatal(err)
	}
	path := tr.Path()
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"checkpoint","checkpoint":{"offset":1,"messages":[`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadSubAgentSnapshot(dir, "agt-checkpoint")
	if err != nil || !reflect.DeepEqual(snap.Messages, history) {
		t.Fatalf("last synced snapshot lost: %#v %v", snap, err)
	}
	resumed, err := ResumeSubAgentTranscript(dir, "agt-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	history = append(history, checkpointMessage("after crash"))
	if err := resumed.Checkpoint(history); err != nil {
		t.Fatal(err)
	}
	snap, err = LoadSubAgentSnapshot(dir, "agt-checkpoint")
	if err != nil || !reflect.DeepEqual(snap.Messages, history) {
		t.Fatalf("trim/replay failed: %#v %v", snap, err)
	}
}

func TestSubAgentCheckpointSyncFailureStopsProviderAndDoesNotAck(t *testing.T) {
	dir, tr := newCheckpointTranscript(t)
	s, record, binding := durablePeerFixture(t)
	p := &durablePeerProvider{}
	l := NewLoop(p, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "system", 4)
	l.DurablePeer, l.HistoryCheckpoint = binding, tr.Checkpoint
	l.AppendUser("process mail")
	if err := tr.Checkpoint(l.History()); err != nil {
		t.Fatal(err)
	}
	syncFailure := errors.New("injected fsync failure")
	tr.syncFile = func() error {
		snap, err := LoadSubAgentSnapshot(dir, "agt-checkpoint")
		if err != nil {
			return err
		}
		if historyEnvelopeCount(snap.Messages) > 0 {
			return syncFailure
		}
		return tr.f.Sync()
	}
	err := l.Run(context.Background(), make(chan Event, 128))
	if !errors.Is(err, syncFailure) {
		t.Fatalf("run error=%v", err)
	}
	if len(p.calls) != 0 {
		t.Fatal("provider called after failed checkpoint")
	}
	row, err := s.Get(context.Background(), record.ID)
	if err != nil || row.State != "pending" || row.CheckpointSHA256 != "" || row.Attempts != 1 {
		t.Fatalf("ack after sync failure: %#v %v", row, err)
	}
	if err := tr.Checkpoint(nil); !errors.Is(err, syncFailure) {
		t.Fatalf("failed writer did not fail closed: %v", err)
	}
	if historyEnvelopeCount(l.History()) != 0 {
		t.Fatal("failed envelope retained in live history")
	}
}

func TestSubAgentCheckpointCrashBeforeAckResumeDoesNotDuplicate(t *testing.T) {
	dir, tr := newCheckpointTranscript(t)
	s, record, binding := durablePeerFixture(t)
	first := &Loop{DurablePeer: binding, Messages: []llm.Message{checkpointMessage("original prompt")}}
	first.HistoryCheckpoint = func(h []llm.Message) error {
		if err := tr.Checkpoint(h); err != nil {
			return err
		}
		row, err := s.Get(context.Background(), record.ID)
		if err != nil {
			return err
		}
		return s.Release(context.Background(), row.ID, row.Token)
	}
	if err := first.injectPeerMessages(context.Background(), nil); !errors.Is(err, handoff.ErrLease) {
		t.Fatalf("lost ack error=%v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadSubAgentSnapshot(dir, "agt-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := ResumeSubAgentTranscript(dir, "agt-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	newBinding, err := NewDurablePeerInbox(s, binding.Scope, binding.Recipient)
	if err != nil {
		t.Fatal(err)
	}
	l := &Loop{Messages: snap.Messages, DurablePeer: newBinding, HistoryCheckpoint: resumed.Checkpoint}
	if err := l.injectPeerMessages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSubAgentSnapshot(dir, "agt-checkpoint")
	if err != nil || historyEnvelopeCount(loaded.Messages) != 1 {
		t.Fatalf("replay duplicated envelope: %#v %v", loaded, err)
	}
	encoded, err := json.Marshal(loaded.Messages)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	row, err := s.Get(context.Background(), record.ID)
	if err != nil || row.State != "acknowledged" || row.Attempts != 2 || row.CheckpointSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("exact checkpoint receipt=%#v %v", row, err)
	}
}

func TestSubAgentCheckpointConcurrentProducerAndLifecycleConsumer(t *testing.T) {
	dir, tr := newCheckpointTranscript(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 80; i++ {
			if err := tr.Checkpoint([]llm.Message{checkpointMessage(strings.Repeat("x", i+1))}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 80; i++ {
			if err := tr.AppendTerminal(SubAgentTerminal{Status: "completed"}); err != nil {
				t.Error(err)
				return
			}
			if _, err := LoadSubAgentSnapshot(dir, "agt-checkpoint"); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	final := []llm.Message{checkpointMessage("final exact producer history")}
	if err := tr.Checkpoint(final); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadSubAgentSnapshot(dir, "agt-checkpoint")
	if err != nil || !reflect.DeepEqual(snap.Messages, final) {
		t.Fatalf("concurrent transcript=%#v %v", snap, err)
	}
}

func TestSubAgentCheckpointClosedTranscriptFails(t *testing.T) {
	_, tr := newCheckpointTranscript(t)
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Checkpoint([]llm.Message{checkpointMessage("not saved")}); err == nil {
		t.Fatal("closed transcript accepted checkpoint")
	}
	if err := tr.BindHandoff("scope", "alice"); err == nil {
		t.Fatal("closed transcript accepted binding")
	}
}

func TestSubAgentDurableCreateNeverReplacesExistingHistory(t *testing.T) {
	dir, tr := newCheckpointTranscript(t)
	if err := tr.Checkpoint([]llm.Message{checkpointMessage("acknowledged history")}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(tr.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if replacement, err := NewDurableSubAgentTranscript(dir, "agt-checkpoint", NewSubAgentHeader("agt-checkpoint", "other", "parent", "alice", "", "bypass")); err == nil {
		_ = replacement.Close()
		t.Fatal("fresh create replaced existing transcript")
	}
	after, err := os.ReadFile(tr.Path())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("existing history changed: %v", err)
	}
}

func TestSubAgentTranscriptWriterLeaseProcess(t *testing.T) {
	if dir := os.Getenv("METIS_TEST_CHILD_TRANSCRIPT_LEASE_DIR"); dir != "" {
		tr, err := ResumeSubAgentTranscript(dir, "agt-checkpoint")
		if err != nil {
			t.Fatal(err)
		}
		defer tr.Close()
		if err := os.WriteFile(filepath.Join(dir, "producer-ready"), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	dir, tr := newCheckpointTranscript(t)
	if err := tr.Checkpoint([]llm.Message{checkpointMessage("acknowledged history")}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(tr.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSubAgentTranscriptWriterLeaseProcess$")
	cmd.Env = append(os.Environ(), "METIS_TEST_CHILD_TRANSCRIPT_LEASE_DIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	defer func() { _ = cmd.Process.Kill() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "producer-ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("writer helper not ready: %s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if second, err := ResumeSubAgentTranscript(dir, "agt-checkpoint"); !errors.Is(err, ErrSubAgentTranscriptBusy) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second producer did not fail busy: %v", err)
	}
	if second, err := NewDurableSubAgentTranscript(dir, "agt-checkpoint", NewSubAgentHeader("agt-checkpoint", "m", "parent", "alice", "", "bypass")); !errors.Is(err, ErrSubAgentTranscriptBusy) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("new producer truncated active transcript: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, SubAgentTranscriptDirname, "agt-checkpoint.jsonl"))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("busy open changed acknowledged bytes: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	resumed, err := ResumeSubAgentTranscript(dir, "agt-checkpoint")
	if err != nil {
		t.Fatalf("SIGKILL left permanent producer lock: %v", err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
	closedLease, err := ResumeSubAgentTranscript(dir, "agt-checkpoint")
	if err != nil {
		t.Fatalf("Close did not release producer lease: %v", err)
	}
	_ = closedLease.Close()
}
