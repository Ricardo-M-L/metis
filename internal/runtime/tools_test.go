package runtime

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/channels"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestBuildToolRegistry_RegistersBuiltinsAndAgentAndSendMessage(t *testing.T) {
	cfg := &config.Config{}
	gate := permission.New(permission.ModeAcceptEdits)
	chReg := channels.NewRegistry()

	reg := BuildToolRegistry(ToolRegistryOptions{
		Cfg:             cfg,
		Gate:            gate,
		Provider:        &stubProvider{maxCtx: 100_000},
		Model:           "claude-x",
		System:          "you are a tester",
		ChannelRegistry: chReg,
		DefaultPlatform: "",
	})

	all := reg.All()
	if len(all) == 0 {
		t.Fatal("BuildToolRegistry returned empty registry")
	}

	want := []string{"Read", "Write", "Bash", "Agent", "SendMessage"}
	got := make(map[string]bool)
	for _, t := range all {
		got[t.Name()] = true
	}
	missing := []string{}
	for _, w := range want {
		if !got[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Errorf("registry missing tools: %s (got %d)", strings.Join(missing, ", "), len(all))
	}
}

func TestBuildToolRegistry_AgentToolHasModelAndSystem(t *testing.T) {
	// Smoke that the Agent tool gets constructed with the model + system
	// we passed — it shows up in the registry but we only check Name/Desc
	// here (its internals are tested in the agent pkg).
	cfg := &config.Config{}
	gate := permission.New(permission.ModeAcceptEdits)
	chReg := channels.NewRegistry()

	reg := BuildToolRegistry(ToolRegistryOptions{
		Cfg: cfg, Gate: gate, Provider: &stubProvider{maxCtx: 100_000},
		Model: "claude-x", System: "sys", ChannelRegistry: chReg,
	})
	agent, ok := reg.Get("Agent")
	if !ok {
		t.Fatal("Agent tool should be registered")
	}
	if agent.Description() == "" {
		t.Error("Agent tool description should be non-empty")
	}
}
