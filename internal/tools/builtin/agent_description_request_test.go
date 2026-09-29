package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type agentDescriptionRequestProvider struct {
	llm.Provider
	specs chan []llm.ToolSpec
}

func (p *agentDescriptionRequestProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	p.specs <- req.Tools
	return p.Provider.Stream(ctx, req)
}

func TestAgentDescriptionCapacityContractReachesProvider(t *testing.T) {
	// In-process Desktop turns do not require process-wide Desktop variables.
	// The provider must still receive the general admission contract in either
	// description mode, without truncation or environment-derived capacities.
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOT_DIR", "")
	for _, short := range []bool{false, true} {
		for _, desktop := range []bool{false, true} {
			t.Run(fmt.Sprintf("short_%t/context_desktop_%t", short, desktop), func(t *testing.T) {
				provider := &agentDescriptionRequestProvider{Provider: helloProvider(), specs: make(chan []llm.ToolSpec, 1)}
				gate := permission.New(permission.ModeBypass)
				registry := tools.NewRegistry()
				tool := NewAgent(gate, provider, registry, "model", "system")
				registry.Register(tool)
				loop := agent.NewLoop(provider, registry, gate, agent.NewHookRegistry(), "system", 2)
				loop.ShortToolDescriptions = short
				loop.AppendUser("inspect tool capabilities")
				ctx := context.Background()
				if desktop {
					ctx = agent.WithDesktopExecutionConfig(ctx, agent.DesktopExecutionConfig{
						SlotDir: t.TempDir(), TotalAgentSlots: 2, SubagentsPerRoot: 1, Owner: "description-request",
					})
				}
				if err := loop.Run(ctx, make(chan agent.Event, 32)); err != nil {
					t.Fatal(err)
				}
				var description string
				for _, spec := range <-provider.specs {
					if spec.Name == "Agent" {
						description = spec.Description
					}
				}
				if description == "" || description != tool.ShortDescription() || len(description) > 200 {
					t.Fatalf("provider description differs or exceeds 200 bytes: %q (%d)", description, len(description))
				}
				for _, want := range []string{"fresh sub-agent", "Desktop queues excess work", "background calls return IDs", "queued work runs automatically", "CLI uses configured limits", "wait for results"} {
					if !strings.Contains(description, want) {
						t.Fatalf("provider description lost %q: %s", want, description)
					}
				}
			})
		}
	}
}
