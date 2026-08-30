package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type rebindProvider struct {
	name  string
	model string
	cap   int
}

func (p rebindProvider) Name() string          { return p.name }
func (p rebindProvider) ModelID() string       { return p.model }
func (p rebindProvider) MaxContextTokens() int { return p.cap }
func (p rebindProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, nil
}
func (p rebindProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, nil
}

func TestRebindProviderToolsPreservesRegistryAndUpdatesCapturedState(t *testing.T) {
	reg := tools.NewRegistry()
	gate := permission.New(permission.ModeAsk)
	oldProvider := rebindProvider{name: "old", model: "old-model", cap: 100}
	newProvider := rebindProvider{name: "new", model: "new-model", cap: 200}
	reg.Register(NewAgentWithMinimal(gate, oldProvider, reg, "old-model", "old-system", "old-minimal").WithSessionPersistence(t.TempDir(), "old-session"))
	reg.Register(NewFork(gate, oldProvider, reg))
	reg.Register(NewMetisInfo(gate, nil, nil, nil, reg).WithModel(oldProvider, "old-model"))
	marker := &rebindMarkerTool{}
	reg.Register(marker)

	RebindProviderTools(reg, newProvider, "new-model", "new-system", "new-session")

	agentTool, _ := reg.Get("Agent")
	a := agentTool.(Agent)
	if a.provider != newProvider || a.model != "new-model" || a.system != "new-system" || a.minimalSystem != "new-system" || a.parentSessionID != "new-session" {
		t.Fatalf("Agent was not fully rebound: provider=%v model=%q system=%q minimal=%q parent=%q", a.provider, a.model, a.system, a.minimalSystem, a.parentSessionID)
	}
	forkTool, _ := reg.Get("Fork")
	if got := forkTool.(Fork).provider; got != newProvider {
		t.Fatalf("Fork provider = %v, want new provider", got)
	}
	infoTool, _ := reg.Get("MetisInfo")
	info := infoTool.(MetisInfo)
	if info.provider != newProvider || info.model != "new-model" {
		t.Fatalf("MetisInfo = provider %v model %q", info.provider, info.model)
	}
	if got, ok := reg.Get(marker.Name()); !ok || got != marker {
		t.Fatal("unrelated dynamically registered tool was replaced")
	}
}

func TestRebindAgentPromptsUpdatesFullAndMinimalSystems(t *testing.T) {
	reg := tools.NewRegistry()
	gate := permission.New(permission.ModeAsk)
	provider := rebindProvider{name: "wire", model: "model", cap: 100}
	reg.Register(NewAgentWithMinimal(gate, provider, reg, "model", "provisional-full", "provisional-minimal"))

	RebindAgentPrompts(reg, "visible-full", "visible-minimal")

	tool, ok := reg.Get("Agent")
	if !ok {
		t.Fatal("Agent disappeared during prompt rebind")
	}
	agentTool := tool.(Agent)
	if agentTool.system != "visible-full" || agentTool.minimalSystem != "visible-minimal" {
		t.Fatalf("Agent prompts = %q / %q", agentTool.system, agentTool.minimalSystem)
	}
}

func TestRebindProviderToolsRebuildsMinimalPromptFromRuntimeState(t *testing.T) {
	reg := tools.NewRegistry()
	gate := permission.New(permission.ModeAsk)
	oldProvider := rebindProvider{name: "wire-old", model: "old-model", cap: 100}
	newProvider := rebindProvider{name: "wire-new", model: "new-model", cap: 200}
	reg.Register(NewAgentWithMinimal(gate, oldProvider, reg, "old-model", "old-full", "old-minimal"))
	reg.Register(&rebindMarkerTool{})

	builder := func(ctx AgentPromptBuildContext) string {
		providerName := ""
		if ctx.Provider != nil {
			providerName = ctx.Provider.Name()
		}
		return fmt.Sprintf(
			"profile=%s provider=%s model=%s cwd=%s tools=%s",
			ctx.ProviderName,
			providerName,
			ctx.Model,
			ctx.WorkingDirectory,
			strings.Join(registryNames(ctx.Registry), ","),
		)
	}
	RebindAgentPrompts(reg, "old-full", "old-minimal", AgentRuntimePromptState{
		ProviderName:         "old-profile",
		WorkingDirectory:     "/old-workspace",
		MinimalPromptBuilder: builder,
	})

	RebindProviderTools(reg, newProvider, "new-model", "new-full", "new-session", AgentRuntimePromptState{
		ProviderName:     "new-profile",
		WorkingDirectory: "/new-workspace",
	})

	tool, ok := reg.Get("Agent")
	if !ok {
		t.Fatal("Agent disappeared during provider rebind")
	}
	agentTool := tool.(Agent)
	wantMinimal := "profile=new-profile provider=wire-new model=new-model cwd=/new-workspace tools=Agent,DynamicMarker"
	if agentTool.system != "new-full" || agentTool.minimalSystem != wantMinimal {
		t.Fatalf("Agent prompt pair after provider rebind = full %q minimal %q; want full %q minimal %q", agentTool.system, agentTool.minimalSystem, "new-full", wantMinimal)
	}
}

type rebindMarkerTool struct{ tools.BaseTool }

func (*rebindMarkerTool) Name() string                { return "DynamicMarker" }
func (*rebindMarkerTool) Description() string         { return "test marker" }
func (*rebindMarkerTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (*rebindMarkerTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (*rebindMarkerTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (*rebindMarkerTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "ok"}, nil
}
