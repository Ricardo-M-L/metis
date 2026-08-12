package openai

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm/transport"
)

func TestBuildReturnsResolvedDefaultModel(t *testing.T) {
	result, err := build(transport.BuildOpts{})
	if err != nil {
		t.Fatalf("build() error = %v", err)
	}

	provider, ok := result.Provider.(*OpenAI)
	if !ok {
		t.Fatalf("build() provider type = %T, want *OpenAI", result.Provider)
	}
	if result.Model != provider.Model {
		t.Fatalf("build() model = %q, provider model = %q", result.Model, provider.Model)
	}
}
