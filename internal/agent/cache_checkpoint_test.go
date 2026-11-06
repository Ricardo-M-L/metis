package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
)

func TestDynamicCheckpointResumeReconstructionAndManagedMode(t *testing.T) {
	p := openai.NewResponses("", "https://api.openai.com/v1", "fixture", 64, 0, 0)
	p.StateMode = openai.ResponsesStateLocal
	state := RuntimeStateSnapshot{SessionID: "fixture", WorkingDirectory: "/one", PermissionMode: "default"}
	l := &Loop{Provider: p, System: "base", SystemSections: []llm.SystemSection{{Name: "base", Body: "base", Cache: true}}, CurrentStateSnapshot: func() RuntimeStateSnapshot { return state }, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}}}
	first := l.buildRequest(nil)
	history := l.History()
	state.WorkingDirectory = "/two"
	second := l.buildRequest(nil)
	if !reflect.DeepEqual(second.Messages[:len(first.Messages)], first.Messages) {
		t.Fatal("changed state mutated previously sent prefix")
	}
	last := second.Messages[len(second.Messages)-1]
	if last.Role != llm.RoleSystem || !strings.Contains(last.Content[0].Text, "/two") {
		t.Fatal("snapshot lost privileged role/current state")
	}
	// Simulate resume from an older persisted checkpoint and current runtime.
	l.Messages = history
	rebuilt := l.buildRequest(nil)
	if len(rebuilt.Messages) != len(history)+1 || !strings.Contains(rebuilt.Messages[len(rebuilt.Messages)-1].Content[0].Text, "/two") {
		t.Fatal("resume failed to reconstruct latest snapshot")
	}
	// Compaction removed all checkpoints: reconstruction must not depend on an
	// in-memory last-hash field from a prior request.
	l.Messages = l.Messages[:1]
	if req := l.buildRequest(nil); len(req.Messages) != 2 {
		t.Fatal("compaction suppressed snapshot reconstruction")
	}
	p.StateMode = openai.ResponsesStateProvider
	count := len(l.Messages)
	for i := 0; i < 3; i++ {
		l.buildRequest(nil)
	}
	if len(l.Messages) != count {
		t.Fatal("managed mode accumulated snapshots")
	}
}
