package main

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// TestBuildListingRegistry_ContractShape locks the invariant `metis schema`
// depends on: every advertised tool has a name + a JSON-Schema input the
// client SDK can consume. If a tool ships an empty/typeless schema, the
// generated contract is broken and this fails before users see it.
func TestBuildListingRegistry_ContractShape(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir()) // isolate from the dev's real ~/.metis
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	reg := buildListingRegistry(cfg, "", "")
	all := reg.All()
	if len(all) < 10 {
		t.Fatalf("expected the full builtin surface, got only %d tools", len(all))
	}
	for _, tool := range all {
		if tool.Name() == "" {
			t.Error("tool with empty name in contract")
		}
		sch := tool.InputSchema()
		if sch == nil {
			t.Errorf("tool %q has nil InputSchema — breaks the generated contract", tool.Name())
			continue
		}
		// A usable JSON Schema is an object schema (type:object and/or
		// properties) — what an SDK client needs to build a typed call.
		if sch["type"] != "object" && sch["properties"] == nil {
			t.Errorf("tool %q InputSchema is not an object schema: %v", tool.Name(), sch)
		}
	}

	// Visibility filter flows through to the contract.
	if got := len(buildListingRegistry(cfg, "Read,Bash", "").All()); got != 2 {
		t.Errorf("--tools filter: want 2 tools, got %d", got)
	}
}
