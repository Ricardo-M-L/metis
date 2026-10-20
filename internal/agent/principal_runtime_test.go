package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestPrincipalIdentitySurvivesPromptRebindAndAffectsContextEstimate(t *testing.T) {
	for _, typed := range []bool{false, true} {
		name := "legacy"
		if typed {
			name = "typed"
		}
		t.Run(name, func(t *testing.T) {
			provider := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 200000}}
			loop := NewLoop(provider, tools.NewRegistry(), nil, nil, "role prompt", 3)
			var sections []llm.SystemSection
			if typed {
				sections = []llm.SystemSection{{Name: "role", Body: "role prompt", Cache: true}}
			}
			loop.RebindProviderRuntime(provider, "same-model", 4096, "role prompt", sections)
			loop.AppendUser("identify your current job")
			before := loop.EstimateRequestContextTokens(nil)
			identity := "job:stable-principal " + strings.Repeat("immutable identity ", 100)
			loop.SetPrincipalIdentity(identity)
			if loop.EstimateRequestContextTokens(nil) <= before {
				t.Fatal("principal identity omitted from context estimate")
			}
			loop.RebindProviderRuntime(provider, "same-model", 4096, "refreshed role prompt", sections)
			request := loop.buildRequest(nil)
			if strings.Count(request.System, identity) != 1 {
				t.Fatal("prompt refresh lost or duplicated principal identity")
			}
			found := 0
			for _, section := range request.SystemSections {
				if section.Name == "principal_identity" && section.Body == identity {
					found++
				}
			}
			if typed && found != 1 {
				t.Fatalf("typed principal identity sections = %d", found)
			}
		})
	}
}

type principalAuthorityTestObserver struct{}

func (principalAuthorityTestObserver) Begin(context.Context, transport.RequestMetadata) (transport.RequestUsageObserver, error) {
	return nil, errors.New("this unit provider does not use HTTP")
}

func TestPhysicalUsageAuthorityDoesNotCreateLegacySpendFloor(t *testing.T) {
	provider := &liveContextCapabilitiesProvider{capabilities: llm.ContextCapabilities{Window: 200000}}
	loop := NewLoop(provider, tools.NewRegistry(), nil, nil, "role", 2)
	loop.Budget = budget.NewTracker(0, budget.Rates{InputPerMTok: 1000000, OutputPerMTok: 1000000})
	legacyWrites := 0
	loop.Budget.SetPersistence(func(float64) error { legacyWrites++; return nil })
	loop.Budget.SetObservedSpend(1)
	loop.AppendUser("respond once")
	ctx := transport.WithUsageObserver(context.Background(), principalAuthorityTestObserver{})
	events := make(chan Event, 32)
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx, events); close(events) }()
	for range events {
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if legacyWrites != 0 {
		t.Fatalf("observed request created %d duplicate legacy charges", legacyWrites)
	}
	loop.Budget.SetObservedSpend(0.2)
	if got := loop.Budget.SpentUSD(); got != 0.2 {
		t.Fatalf("authoritative correction blocked by legacy floor: %v", got)
	}
}
