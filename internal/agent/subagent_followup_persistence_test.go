package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestSubAgentFollowupDurableOrderedResumeAndConsumption(t *testing.T) {
	dir, writer := newCheckpointTranscript(t)
	first, err := writer.EnqueueFollowup(" first instruction \n")
	if err != nil {
		t.Fatal(err)
	}
	second, err := writer.EnqueueFollowup("second instruction")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.Text != "first instruction" {
		t.Fatalf("follow-up identity = %#v, %#v", first, second)
	}
	before, _ := os.ReadFile(writer.Path())
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSubAgentSnapshot(dir, "agt-checkpoint")
	if err != nil || !reflect.DeepEqual(snapshot.PendingFollowups, []SubAgentFollowup{first, second}) {
		t.Fatalf("pending recovery = %#v, %v", snapshot, err)
	}
	resumed, err := ResumeSubAgentTranscript(dir, "agt-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	pending := resumed.PendingFollowups()
	pending[0].Text = "caller must not mutate queue"
	if !reflect.DeepEqual(resumed.PendingFollowups(), []SubAgentFollowup{first, second}) {
		t.Fatal("pending queue exposed mutable state")
	}
	history := []llm.Message{checkpointMessage(first.UserText())}
	if err := resumed.Checkpoint(history); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resumed.PendingFollowups(), []SubAgentFollowup{second}) {
		t.Fatal("checkpoint did not acknowledge exactly the delivered instruction")
	}
	history = append(history, checkpointMessage("[user steer mid-turn] unrelated steering\n"+second.UserText()+"\nother steering"))
	if err := resumed.Checkpoint(history); err != nil {
		t.Fatal(err)
	}
	if len(resumed.PendingFollowups()) != 0 {
		t.Fatal("combined steering did not acknowledge delivery")
	}
	after, _ := os.ReadFile(resumed.Path())
	if !bytes.HasPrefix(after, before) || strings.Count(string(after), `"type":"followup_ack"`) != 2 {
		t.Fatal("acknowledgements rewrote or duplicated the durable instruction log")
	}
	snapshot, err = LoadSubAgentSnapshot(dir, "agt-checkpoint")
	if err != nil || len(snapshot.PendingFollowups) != 0 || !reflect.DeepEqual(snapshot.Messages, history) {
		t.Fatalf("consumed recovery = %#v, %v", snapshot, err)
	}
	conversation, err := LoadSubAgentConversation(dir, "agt-checkpoint")
	if err != nil || !reflect.DeepEqual(conversation.Messages, history) {
		t.Fatalf("conversation metadata projection = %#v, %v", conversation, err)
	}
}

func TestSubAgentFollowupCheckpointBeforeAckCrashDoesNotRedeliver(t *testing.T) {
	dir, writer := newCheckpointTranscript(t)
	followup, err := writer.EnqueueFollowup("preserve once")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the crash interval after a complete history checkpoint but
	// before the explicit follow-up acknowledgement reaches disk.
	if err := writer.writeEntry(subagentEntry{Type: "checkpoint", Checkpoint: &subagentHistoryCheckpoint{Offset: 0, Messages: []llm.Message{checkpointMessage(followup.UserText())}}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSubAgentSnapshot(dir, "agt-checkpoint")
	if err != nil || len(snapshot.PendingFollowups) != 0 {
		t.Fatalf("checkpointed instruction redelivered: %#v, %v", snapshot, err)
	}
	resumed, err := ResumeSubAgentTranscript(dir, "agt-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if len(resumed.PendingFollowups()) != 0 {
		t.Fatal("resumed writer redelivered checkpointed instruction")
	}
}

func TestSubAgentFollowupUnconsumedOnFalseMarkerAndSyncFailure(t *testing.T) {
	for _, role := range []llm.Role{llm.RoleAssistant, llm.RoleTool, llm.RoleUser} {
		t.Run(string(role), func(t *testing.T) {
			dir, writer := newCheckpointTranscript(t)
			followup, err := writer.EnqueueFollowup("whole instruction")
			if err != nil {
				t.Fatal(err)
			}
			text := followup.UserText()
			if role == llm.RoleUser {
				text += " is quoted by a different message"
			}
			message := checkpointMessage(text)
			message.Role = role
			if err := writer.Checkpoint([]llm.Message{message}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := LoadSubAgentSnapshot(dir, "agt-checkpoint")
			if err != nil || len(snapshot.PendingFollowups) != 1 || len(writer.PendingFollowups()) != 1 {
				t.Fatalf("non-delivery acknowledged: %#v, %v", snapshot, err)
			}
		})
	}
	t.Run("failed sync never returns acceptance", func(t *testing.T) {
		_, writer := newCheckpointTranscript(t)
		writer.syncFile = func() error { return errors.New("fixture sync failure") }
		followup, err := writer.EnqueueFollowup("not acknowledged")
		if err == nil || followup.ID != "" || len(writer.PendingFollowups()) != 0 {
			t.Fatalf("failed sync acknowledged instruction: %#v, %v", followup, err)
		}
	})
}

func TestSubAgentFollowupMalformedMetadataFailsBothReaders(t *testing.T) {
	dir, writer := newCheckpointTranscript(t)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(writer.Path(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewEncoder(file).Encode(subagentEntry{Type: "followup_ack", FollowupAck: &subAgentFollowupAck{ID: "never-queued"}})
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSubAgentSnapshot(dir, "agt-checkpoint"); err == nil {
		t.Fatal("snapshot accepted acknowledgement without instruction")
	}
	if _, err := LoadSubAgentConversation(dir, "agt-checkpoint"); err == nil {
		t.Fatal("conversation accepted acknowledgement without instruction")
	}
}

func TestSubAgentFollowupConcurrentParentSubmissionsHaveOneDurableOrder(t *testing.T) {
	dir, writer := newCheckpointTranscript(t)
	const count = 24
	var group sync.WaitGroup
	results := make(chan SubAgentFollowup, count)
	errors := make(chan error, count)
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			followup, err := writer.EnqueueFollowup("same text is still a distinct instruction")
			if err != nil {
				errors <- err
				return
			}
			results <- followup
		}()
	}
	group.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	ids := make(map[string]bool, count)
	for followup := range results {
		ids[followup.ID] = true
	}
	if len(ids) != count {
		t.Fatalf("concurrent acceptance IDs = %d, want %d", len(ids), count)
	}
	ordered := writer.PendingFollowups()
	snapshot, err := LoadSubAgentSnapshot(dir, "agt-checkpoint")
	if err != nil || !reflect.DeepEqual(snapshot.PendingFollowups, ordered) {
		t.Fatalf("disk and live queue disagree: %#v, %v", snapshot, err)
	}
	history := make([]llm.Message, 0, count)
	for _, followup := range ordered {
		history = append(history, checkpointMessage(followup.UserText()))
	}
	if err := writer.Checkpoint(history); err != nil {
		t.Fatal(err)
	}
	snapshot, err = LoadSubAgentSnapshot(dir, "agt-checkpoint")
	if err != nil || len(snapshot.PendingFollowups) != 0 || len(snapshot.Messages) != count {
		t.Fatalf("concurrent instruction checkpoint = %#v, %v", snapshot, err)
	}
}
