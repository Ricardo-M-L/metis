package builtin

import (
	"context"
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
	reg.Register(NewAgent(gate, oldProvider, reg, "old-model", "old-system").WithSessionPersistence(t.TempDir(), "old-session"))
	reg.Register(NewFork(gate, oldProvider, reg))
	reg.Register(NewMetisInfo(gate, nil, nil, nil, reg).WithModel(oldProvider, "old-model"))
	marker := &rebindMarkerTool{}
	reg.Register(marker)

	RebindProviderTools(reg, newProvider, "new-model", "new-system", "new-session")

	agentTool, _ := reg.Get("Agent")
	a := agentTool.(Agent)
	if a.provider != newProvider || a.model != "new-model" || a.system != "new-system" || a.parentSessionID != "new-session" {
		t.Fatalf("Agent was not fully rebound: provider=%v model=%q system=%q parent=%q", a.provider, a.model, a.system, a.parentSessionID)
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
