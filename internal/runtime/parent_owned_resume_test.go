package runtime

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestPrepareResumeRejectsParentOwnedChildAsRoot(t *testing.T) {
	store := newResumeStore(t)
	if err := store.WriteHeaderFull(session.Header{ID: "legacy-child", SubAgentOf: "original-parent", Model: "child-model"}); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareResume(store, "legacy-child"); err == nil || !strings.Contains(err.Error(), "original-parent") {
		t.Fatalf("child resumed as root: %v", err)
	}
}

func TestApplyPreparedResumeRejectsParentOwnedChildBeforeMutation(t *testing.T) {
	gate := permission.New(permission.ModePlan)
	loop := agent.NewLoop(nil, tools.NewRegistry(), gate, nil, "parent-system", 5)
	prepared := &PreparedResume{SessionID: "child", Header: &session.Header{ID: "child", SubAgentOf: "parent", Mode: "fullAccess"}, messages: []llm.Message{{Role: llm.RoleUser}}}
	if _, err := ApplyPreparedResume(prepared, loop, gate, nil); err == nil || !strings.Contains(err.Error(), "parent") {
		t.Fatalf("child applied as root: %v", err)
	}
	if gate.Mode() != permission.ModePlan || len(loop.Messages) != 0 {
		t.Fatal("blocked child resume changed root permission or history")
	}
}

func TestPrepareResumeRejectsChildIDShadowedByRootRecord(t *testing.T) {
	store := newResumeStore(t)
	if err := store.WriteHeaderFull(session.Header{ID: "child", Model: "main-model"}); err != nil {
		t.Fatal(err)
	}
	transcript, err := agent.NewDurableSubAgentTranscript(store.Dir, "child", session.Header{ID: "child", SubAgentOf: "original-parent", Model: "child-model"})
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareResume(store, "child"); err == nil || !strings.Contains(err.Error(), "original-parent") {
		t.Fatalf("shadow root record bypassed child ownership: %v", err)
	}
}
