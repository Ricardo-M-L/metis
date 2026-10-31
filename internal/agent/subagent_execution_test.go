package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func executionFixture(t *testing.T) SubAgentExecutionConfig {
	t.Helper()
	return SubAgentExecutionConfig{
		Version: 1, Provider: "configured-provider", Model: "original-model",
		Effort: "high", Fast: true, WorkDir: t.TempDir(), System: strings.Repeat("original system\n", 8192),
		Profile: "reviewer", Tools: []string{"Read", "Glob"}, PermissionMode: "plan",
		PermissionRulesHash: "fixture-rules-digest",
	}
}

func TestSubAgentExecutionRoundtripAndImmutableResume(t *testing.T) {
	dir := t.TempDir()
	config := executionFixture(t)
	writer, err := NewDurableSubAgentTranscript(dir, "agt-identity", NewSubAgentHeader("agt-identity", config.Model, "parent", "reviewer", config.WorkDir, config.PermissionMode))
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AppendExecution(config); err != nil {
		t.Fatal(err)
	}
	if err := writer.Checkpoint(makeFixtureMessages()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(writer.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AppendExecution(config); err != nil {
		t.Fatal(err)
	}
	idempotent, _ := os.ReadFile(writer.Path())
	if !bytes.Equal(before, idempotent) {
		t.Fatal("repeated binding duplicated execution history")
	}
	config.Tools[0] = "Write"
	if err := writer.AppendExecution(config); err == nil {
		t.Fatal("caller mutation changed bound identity")
	}
	config.Tools[0] = "Read"
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSubAgentSnapshot(dir, "agt-identity")
	if err != nil || snapshot.Execution == nil || !reflect.DeepEqual(*snapshot.Execution, config) {
		t.Fatalf("execution roundtrip = %#v, %v", snapshot, err)
	}
	// The large system prompt belongs in its own record, not the bounded
	// first-header read used by Desktop ownership checks.
	header, err := LoadSubAgentHeader(dir, "agt-identity")
	if err != nil || header.SubAgentOf != "parent" {
		t.Fatalf("bounded header = %#v, %v", header, err)
	}
	conversation, err := LoadSubAgentConversation(dir, "agt-identity")
	if err != nil || !reflect.DeepEqual(conversation.Messages, makeFixtureMessages()) {
		t.Fatalf("identity leaked into conversation messages: %#v, %v", conversation, err)
	}
	resumed, err := ResumeSubAgentTranscript(dir, "agt-identity")
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	config.Model = "parent-selected-new-model"
	if err := resumed.AppendExecution(config); err == nil {
		t.Fatal("resume accepted a new execution identity")
	}
	if err := resumed.Checkpoint(append(makeFixtureMessages(), checkpointMessage("follow-up"))); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(resumed.Path())
	if !bytes.HasPrefix(after, before) {
		t.Fatal("resume replaced acknowledged identity or history")
	}
}

func TestSubAgentExecutionLegacyAndCorruptRecords(t *testing.T) {
	t.Run("legacy has no fabricated identity", func(t *testing.T) {
		dir, writer := newCheckpointTranscript(t)
		if err := writer.Checkpoint(makeFixtureMessages()); err != nil {
			t.Fatal(err)
		}
		snapshot, err := LoadSubAgentSnapshot(dir, "agt-checkpoint")
		if err != nil || snapshot.Execution != nil {
			t.Fatalf("legacy identity = %#v, %v", snapshot, err)
		}
	})
	for _, mutation := range []string{"unsupported version", "changed identity", "missing identity", "implicit tool inheritance"} {
		t.Run(mutation, func(t *testing.T) {
			dir, writer := newCheckpointTranscript(t)
			config := executionFixture(t)
			if err := writer.AppendExecution(config); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "unsupported version":
				config.Version = 2
			case "changed identity":
				config.PermissionMode = "fullAccess"
			case "implicit tool inheritance":
				config.Tools = nil
			}
			entry := subagentEntry{Type: "execution", Execution: &config}
			if mutation == "missing identity" {
				entry.Execution = nil
			}
			file, err := os.OpenFile(writer.Path(), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			err = json.NewEncoder(file).Encode(entry)
			_ = file.Close()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadSubAgentSnapshot(dir, "agt-checkpoint"); err == nil {
				t.Fatal("snapshot accepted corrupt execution identity")
			}
			if _, err := LoadSubAgentConversation(dir, "agt-checkpoint"); err == nil {
				t.Fatal("conversation accepted corrupt execution identity")
			}
		})
	}
}
