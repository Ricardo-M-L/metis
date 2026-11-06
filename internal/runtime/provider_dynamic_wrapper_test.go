package runtime

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

type dynamicWrapperFixture struct {
	llm.Provider
	enabled bool
}

func (p *dynamicWrapperFixture) DynamicContextCheckpoints() bool { return p.enabled }

func TestRuntimeWrappersRetainLiveDynamicCheckpointCapability(t *testing.T) {
	base := &dynamicWrapperFixture{}
	wrapped := withVisionOverride(providerModelSettingsProvider{Provider: base}, true)
	wrapped = llm.BindExecutionProvider(wrapped, context.Background())
	capability, ok := wrapped.(interface{ DynamicContextCheckpoints() bool })
	if !ok {
		t.Fatal("runtime/execution wrapper chain lost capability")
	}
	for _, enabled := range []bool{false, true, false} {
		base.enabled = enabled
		if capability.DynamicContextCheckpoints() != enabled {
			t.Fatal("wrapper cached stale capability")
		}
	}
	for _, provider := range []llm.Provider{withVisionOverride(providerModelSettingsProvider{}, true), providerModelSettingsProvider{}} {
		if provider.(interface{ DynamicContextCheckpoints() bool }).DynamicContextCheckpoints() {
			t.Fatal("absent capability invented support")
		}
	}
}
