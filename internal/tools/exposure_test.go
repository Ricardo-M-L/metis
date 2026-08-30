package tools

import "testing"

type exposureFakeTool struct {
	fakeTool
	exposure ToolExposure
}

func (t exposureFakeTool) ToolExposure() ToolExposure { return t.exposure }

type disabledFakeTool struct{ fakeTool }

func (disabledFakeTool) IsEnabled() bool { return false }

func TestRegistryModelEntriesUseExplicitExposureAndLegacyMCPFallback(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "Read"})
	r.Register(fakeTool{name: "mcp__legacy__search"})
	r.Register(exposureFakeTool{fakeTool: fakeTool{name: "DeferredBuiltin"}, exposure: ToolExposureDeferred})
	r.Register(exposureFakeTool{fakeTool: fakeTool{name: "InternalOnly"}, exposure: ToolExposureHidden})

	entries := r.ModelEntriesForCache()
	got := make(map[string]ToolExposure, len(entries))
	for _, entry := range entries {
		got[entry.Tool.Name()] = entry.Exposure
	}
	if got["Read"] != ToolExposureDirect {
		t.Fatalf("Read exposure = %q, want direct", got["Read"])
	}
	if got["mcp__legacy__search"] != ToolExposureDeferred {
		t.Fatalf("legacy MCP exposure = %q, want deferred", got["mcp__legacy__search"])
	}
	if got["DeferredBuiltin"] != ToolExposureDeferred {
		t.Fatalf("explicit deferred exposure = %q", got["DeferredBuiltin"])
	}
	if _, ok := got["InternalOnly"]; ok {
		t.Fatal("hidden tool leaked into the model-facing registry view")
	}
	if _, ok := r.Get("InternalOnly"); !ok {
		t.Fatal("hidden tool should remain available to internal raw lookup")
	}
	if _, ok := r.GetForModel("InternalOnly"); ok {
		t.Fatal("hidden tool should not resolve through model-facing lookup")
	}
}

func TestRegistryRejectsDisabledToolsAtEveryRegistrationPath(t *testing.T) {
	r := NewRegistry()
	r.Register(disabledFakeTool{fakeTool{name: "DisabledRegister"}})
	r.Replace(disabledFakeTool{fakeTool{name: "DisabledReplace"}})
	r.ReplacePrefix("mcp__late__", []Tool{
		disabledFakeTool{fakeTool{name: "mcp__late__disabled"}},
	})

	for _, name := range []string{"DisabledRegister", "DisabledReplace", "mcp__late__disabled"} {
		if _, ok := r.Get(name); ok {
			t.Fatalf("disabled tool %q was registered", name)
		}
	}
}
