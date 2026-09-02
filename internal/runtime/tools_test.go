package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/channels"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
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

func TestBuildToolRegistry_AgentSchemaPublishesAvailableProfileEnum(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(project)
	projectAgents := filepath.Join(project, ".metis", "agents")
	if err := os.MkdirAll(projectAgents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectAgents, "custom-scout.md"), []byte("Custom scout"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := BuildToolRegistry(ToolRegistryOptions{
		Cfg:             &config.Config{},
		Gate:            permission.New(permission.ModeAcceptEdits),
		Provider:        &stubProvider{maxCtx: 100_000},
		Model:           "claude-x",
		System:          "sys",
		ChannelRegistry: channels.NewRegistry(),
	})
	agentTool, ok := reg.Get("Agent")
	if !ok {
		t.Fatal("Agent tool should be registered")
	}
	properties := agentTool.InputSchema()["properties"].(map[string]any)
	profileSchema := properties["subagent_type"].(map[string]any)
	profiles, ok := profileSchema["enum"].([]string)
	if !ok {
		t.Fatalf("subagent_type enum type = %T, want []string", profileSchema["enum"])
	}
	for _, want := range []string{"custom-scout", "explore", "general", "verify"} {
		if !slices.Contains(profiles, want) {
			t.Fatalf("Agent profile enum %v missing %q", profiles, want)
		}
	}
	if slices.Contains(profiles, "research") {
		t.Fatalf("Agent profile enum unexpectedly advertises unavailable profile research: %v", profiles)
	}
}

func TestBuildToolRegistryInjectsSandboxIntoMonitor(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	pool := jobs.NewRegistry(t.TempDir())
	t.Cleanup(func() { pool.Shutdown(0) })

	reg := BuildToolRegistry(ToolRegistryOptions{
		Cfg:             &config.Config{},
		Gate:            permission.New(permission.ModeBypassPermissions),
		Provider:        &stubProvider{maxCtx: 100_000},
		Model:           "test-model",
		System:          "test-system",
		ChannelRegistry: channels.NewRegistry(),
		Sandbox:         manager,
		Jobs:            pool,
		Monitors:        agent.NewMonitorRegistry(1),
	})
	t.Cleanup(func() {
		if monitor, ok := reg.Get("Monitor"); ok {
			if typed, ok := monitor.(builtin.Monitor); ok {
				typed.Watches.StopAll()
			}
		}
	})

	registered, ok := reg.Get("Monitor")
	if !ok {
		t.Fatal("Monitor tool was not registered")
	}
	monitor, ok := registered.(builtin.Monitor)
	if !ok {
		t.Fatalf("Monitor registration type = %T", registered)
	}
	if monitor.SandboxManager() != manager {
		t.Fatal("BuildToolRegistry did not inject its sandbox Manager into Monitor")
	}
}
