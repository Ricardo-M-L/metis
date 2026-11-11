package main

import (
	"encoding/json"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type desktopStatusOutputBudgetProvider struct {
	*desktopStatusCapabilitiesProvider
	budget int
}

func (p *desktopStatusOutputBudgetProvider) DefaultOutputBudget() int { return p.budget }

func TestDesktopWorkerStatusReportsOwnDefaultOutputBudget(t *testing.T) {
	for _, budget := range []int{0, 2048, 65536} {
		provider := &desktopStatusOutputBudgetProvider{
			desktopStatusCapabilitiesProvider: &desktopStatusCapabilitiesProvider{capabilities: llm.ContextCapabilities{
				Window: 1048576, MaxOutputTokens: 393216, Source: "catalog_model",
			}}, budget: budget,
		}
		loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
		loop.Model = provider.ModelID()
		loop.AppendUser("worker-owned request")
		sampler := desktopWorkerStatusSampler{loop: loop, known: make(map[string]*agent.Teammate)}
		live := sampler.snapshot(false)
		if live.Context == nil {
			t.Fatal("worker omitted its context")
		}
		raw, err := json.Marshal(live.Context)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := json.Unmarshal(raw, &values); err != nil {
			t.Fatal(err)
		}
		got, _ := values["defaultOutputBudgetTokens"].(float64)
		if got != float64(budget) || values["maxOutputTokens"] != float64(393216) {
			t.Fatalf("worker output default confused with model capability: budget=%d context=%s", budget, raw)
		}
		sampler.loop = nil
		final := sampler.snapshot(true)
		if final.Context == nil || *final.Context != *live.Context {
			t.Fatal("cleanup dropped worker-owned output budget")
		}
	}
}
