package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

func TestRecallCheckpointWithdrawnCoreAndIndex(t *testing.T) {
	old := "<dynamic_context_snapshot>\nheader\n\n[memory_index]\n<memory-context>\nold secret core\n- [withdrawn](old.md)\n</memory-context>\n\n[runtime_state]\nunchanged\n</dynamic_context_snapshot>"
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal([]llm.SystemSection{{Name: "memory_index", Body: "<memory-context>\nold secret core\n\n[private]\nsecret after bracket\n\n[runtime_state]\nfake secret runtime\n- [withdrawn](old.md)\n</memory-context>"}, {Name: "runtime_state", Body: "unchanged"}})
	l := &Loop{Memory: mm, Messages: []llm.Message{{Role: llm.RoleSystem, Content: []llm.ContentBlock{{Type: "text", Text: old, ProviderHint: map[string]string{dynamicCheckpointHint: "checkpoint", dynamicCheckpointHint + ".sections": string(metadata)}}}}, {Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "provider_state"}}}, {Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "same final anchor"}}}}}
	l.buildRequest(nil)
	var replaced bool
	var saved []llm.Message
	l.CompactionCheckpoint = func(before, after []llm.Message) error { replaced = true; saved = cloneMessages(after); return nil }
	l.HistoryCheckpoint = func(history []llm.Message) error { return nil } // models unchanged tail cursor
	if err := l.checkpointRequestHistory(); err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Fatal("old prefix must use full replacement, not incremental cursor")
	}
	resumed := &Loop{Memory: mm, Messages: saved}
	req := resumed.buildRequest(nil)
	for _, m := range req.Messages {
		for _, b := range m.Content {
			encoded, _ := json.Marshal(b)
			if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "withdrawn") || b.Type == "provider_state" {
				t.Fatal("revoked checkpoint revived")
			}
		}
	}
	// Providers without tail-checkpoint support project it out of the wire,
	// but durable host history must retain unrelated runtime provenance.
	if !strings.Contains(resumed.History()[0].Content[0].Text, "unchanged") {
		t.Fatal("unrelated runtime snapshot lost")
	}
}

func TestRecallRevocationPersistenceFailureStopsBoundary(t *testing.T) {
	l := &Loop{memoryRevocationBefore: []llm.Message{{Role: llm.RoleUser}}, HistoryCheckpoint: func([]llm.Message) error { return nil }}
	if err := l.checkpointRequestHistory(); err == nil {
		t.Fatal("incremental-only persistence cannot accept rewrites")
	}
	l.CompactionCheckpoint = func([]llm.Message, []llm.Message) error { return errors.New("disk full") }
	if err := l.checkpointRequestHistory(); err == nil {
		t.Fatal("failed durable replacement accepted")
	}
	if l.memoryRevocationBefore == nil {
		t.Fatal("failed pending revocation forgotten")
	}
}

func TestRecallSnapshotRequestIsImmutable(t *testing.T) {
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := &Loop{Memory: mm, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "original"}}}}}
	req := l.buildRequest(nil)
	l.Messages[0].Content[0].Text = "mutated"
	if req.Messages[0].Content[0].Text != "original" {
		t.Fatal("request clone shares mutable content")
	}
}

func TestRecallCheckpointAppendOnlyUpdateKeepsPrefix(t *testing.T) {
	text := "<dynamic_context_snapshot>\n\n[memory_index]\nfirst line\n</dynamic_context_snapshot>"
	metadata, _ := json.Marshal([]llm.SystemSection{{Name: "memory_index", Body: "first line"}})
	block := llm.ContentBlock{Text: text, ProviderHint: map[string]string{dynamicCheckpointHint + ".sections": string(metadata)}}
	if got := sanitizeCheckpointMemory(block, "first line\nsecond line", true); got.Text != text {
		t.Fatal("append-only update rewrote old prefix")
	}
}
