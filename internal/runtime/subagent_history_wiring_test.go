package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
	pubsess "github.com/Ricardo-M-L/metis/pkg/session"
)

func TestBuildToolRegistrySavedSubAgentReadersBindAndRebindSession(t *testing.T) {
	dir := t.TempDir()
	previous := CurrentSessionID()
	SetCurrentSessionID("parent-one")
	t.Cleanup(func() { SetCurrentSessionID(previous) })
	for _, parent := range []string{"parent-one", "parent-two"} {
		transcript, err := agent.NewSubAgentTranscript(dir, "agt-"+parent, pubsess.Header{ID: "agt-" + parent, SubAgentOf: parent, TeammateName: parent, CreatedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if err = transcript.AppendTerminal(agent.SubAgentTerminal{Status: "completed", Result: parent + "-output", StopHint: "end_turn"}); err != nil {
			t.Fatal(err)
		}
		transcript.Close()
	}
	cfg := &config.Config{}
	cfg.Session.Dir = dir
	provider := &stubProvider{maxCtx: 100_000}
	reg := BuildToolRegistry(ToolRegistryOptions{Cfg: cfg, Gate: permission.New(permission.ModeBypass), Provider: provider, Model: "test", System: "test"})
	for _, name := range []string{"SubAgentOutput", "SubAgentList"} {
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("history reader %s absent without live Roster", name)
		}
	}
	read := func(id string, wantError bool) {
		t.Helper()
		tool, _ := reg.Get("SubAgentOutput")
		result, err := tool.Execute(context.Background(), map[string]any{"agent_id": id})
		if err != nil || result.IsError != wantError || (!wantError && !strings.Contains(result.Output, "load_state=not_loaded")) {
			t.Fatalf("wrong persistence scope for %s: %+v %v", id, result, err)
		}
	}
	read("agt-parent-one", false)
	read("agt-parent-two", true)
	builtin.RebindProviderTools(reg, provider, "test", "test", "parent-two")
	read("agt-parent-one", true)
	read("agt-parent-two", false)
}
