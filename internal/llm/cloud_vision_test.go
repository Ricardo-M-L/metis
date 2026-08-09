package llm_test

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm/azure"
	"github.com/Ricardo-M-L/metis/internal/llm/bedrock"
	"github.com/Ricardo-M-L/metis/internal/llm/vertex"
	pubprov "github.com/Ricardo-M-L/metis/pkg/provider"
)

// TestCloudTransportsExposeVisionCapability keeps the TUI's configured-model
// filter and the provider instance used at submit time on the same decision.
// Without the runtime VisionSupporter methods, selecting one of these models
// succeeds but the next Enter is gated as text-only again.
func TestCloudTransportsExposeVisionCapability(t *testing.T) {
	tests := []struct {
		name string
		prov pubprov.Provider
		want bool
	}{
		{
			name: "azure openai vision deployment",
			prov: &azure.Azure{Model: "gpt-4o-metis-vision-fixture"},
			want: true,
		},
		{
			name: "azure text-only deployment",
			prov: &azure.Azure{Model: "deepseek-v4-flash-metis-fixture"},
			want: false,
		},
		{
			name: "bedrock anthropic inference profile",
			prov: &bedrock.Bedrock{Model: "us.anthropic.claude-sonnet-metis-vision-fixture-v1:0"},
			want: true,
		},
		{
			name: "bedrock non-anthropic model",
			prov: &bedrock.Bedrock{Model: "us.meta.llama-metis-text-fixture-v1:0"},
			want: false,
		},
		{
			name: "vertex anthropic model",
			prov: &vertex.Vertex{Model: "claude-sonnet-metis-vision-fixture@20260101"},
			want: true,
		},
		{
			name: "vertex unknown text model",
			prov: &vertex.Vertex{Model: "text-only-metis-fixture@20260101"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pubprov.ProviderSupportsVision(tt.prov); got != tt.want {
				t.Fatalf("ProviderSupportsVision(%T) = %v, want %v", tt.prov, got, tt.want)
			}
		})
	}
}
