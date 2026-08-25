package webui

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestCommitActiveModelSelectionPersistsSessionHeader(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "model-switch-session"
	if err := store.WriteHeaderFull(session.Header{
		ID: sessionID, Provider: "bigmodel", Model: "glm-5.3", System: "system",
	}); err != nil {
		t.Fatal(err)
	}

	provider := &activationTestProvider{name: "wire", model: "glm-5.3"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = "sensenova-6.8-flash-lite"
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: sessionID,
		ProviderName:     "bigmodel",
	})

	if err := s.commitActiveModelSelection("sensenova", loop.Model); err != nil {
		t.Fatalf("commitActiveModelSelection: %v", err)
	}
	hdr, _, err := store.LoadHeader(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Provider != "sensenova" || hdr.Model != "sensenova-6.8-flash-lite" {
		t.Fatalf("persisted provider/model = %q/%q", hdr.Provider, hdr.Model)
	}
}
