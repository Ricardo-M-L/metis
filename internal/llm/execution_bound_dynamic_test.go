package llm

import (
	"context"
	"testing"
)

type executionDynamicProvider struct {
	*executionTestProvider
	enabled bool
}

func (p *executionDynamicProvider) DynamicContextCheckpoints() bool { return p.enabled }

func TestExecutionBindingRetainsLiveDynamicContextCapability(t *testing.T) {
	base := &executionDynamicProvider{executionTestProvider: &executionTestProvider{}, enabled: true}
	bound := BindExecutionProvider(base, context.Background())
	capability, ok := bound.(interface{ DynamicContextCheckpoints() bool })
	if !ok || !capability.DynamicContextCheckpoints() {
		t.Fatal("execution binding lost checkpoint capability")
	}
	base.enabled = false
	if capability.DynamicContextCheckpoints() {
		t.Fatal("execution binding cached stale mode")
	}
	base.enabled = true
	nested := BindExecutionProvider(bound, context.Background())
	if !nested.(interface{ DynamicContextCheckpoints() bool }).DynamicContextCheckpoints() {
		t.Fatal("nested binding lost checkpoint capability")
	}
	plain := BindExecutionProvider(&executionTestProvider{}, context.Background())
	if plain.(interface{ DynamicContextCheckpoints() bool }).DynamicContextCheckpoints() {
		t.Fatal("plain provider gained checkpoint opt-in")
	}
}
