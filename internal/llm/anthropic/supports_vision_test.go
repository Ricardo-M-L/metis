package anthropic

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestSupportsVision(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model string
		want  bool
	}{
		{"claude-3-5-sonnet-20241022", true},
		{"claude-3-opus-20240229", true},
		{"claude-3-7-sonnet-latest", true},
		{"claude-opus-4-1-20250805", true},
		{"claude-sonnet-4-6-20251115", true},
		{"claude-haiku-4-5-20251001", true},
		{"claude-4-7-opus", true},
		{"claude-2.1", false},
		{"claude-2", false},
		{"claude-instant-1.2", false},
		// MiniMax routes its vision-capable flagship through the
		// api.minimaxi.com/anthropic-compat layer — user confirms the
		// endpoint accepts image content blocks. Allow `minimax-m*` and
		// the explicit `minimax-vl*` variant.
		{"minimax-m2.7", true},
		{"minimax-m2", true},
		{"minimax-vl-01", true},
		// Non-MiniMax non-Claude families don't use this transport.
		{"deepseek-v4-pro", false},
		{"", false},
	}
	for _, c := range cases {
		a := &Anthropic{Model: c.model}
		if got := a.SupportsVision(); got != c.want {
			t.Errorf("SupportsVision(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}

// TestVisionCapabilityForRouteIsRouteScoped pins the 2026-09-18 route-scoping
// for the Anthropic transport. The same wire id is re-published by many
// gateways with different modalities (and by vendored namespaces such as
// Bedrock), so the answer must come from the route in use rather than from a
// sibling that happens to declare image support. Hermetic: uses an explicit
// static client so it never depends on the developer's ~/.metis catalog.
func TestVisionCapabilityForRouteIsRouteScoped(t *testing.T) {
	cli := catalog.NewStaticClient(catalog.Catalog{
		"text-gateway": {Models: map[string]catalog.Model{
			"glm-5.3": {Modalities: catalog.Modalities{Input: []string{"text"}}},
		}},
		"vision-gateway": {Models: map[string]catalog.Model{
			"glm-5.3": {Modalities: catalog.Modalities{Input: []string{"text", "image"}}},
		}},
		"bedrock-route": {Models: map[string]catalog.Model{
			"us.anthropic.claude-sonnet-4-5-20250929-v1:0": {Modalities: catalog.Modalities{Input: []string{"text"}}},
		}},
		"text-only-claude": {Models: map[string]catalog.Model{
			"claude-sonnet-4-6": {Modalities: catalog.Modalities{Input: []string{"text"}}},
		}},
	})

	// (1) The exact route fact is authoritative, in both directions.
	if got := visionCapabilityForRouteWithCatalog("text-gateway", "glm-5.3", cli); got != provider.VisionUnsupported {
		t.Fatalf("text-only route = %v, want VisionUnsupported", got)
	}
	if got := visionCapabilityForRouteWithCatalog("vision-gateway", "glm-5.3", cli); got != provider.VisionSupported {
		t.Fatalf("image route = %v, want VisionSupported", got)
	}
	// A route fact must also override a family-table default: the fallback
	// table calls claude-sonnet vision-capable, this route does not.
	if got := visionCapabilityForRouteWithCatalog("text-only-claude", "claude-sonnet-4-6", cli); got != provider.VisionUnsupported {
		t.Fatalf("route fact should beat the family table; got %v", got)
	}

	// (2) Conflicting routes stay ambiguous instead of borrowing support.
	// glm-5.3 has no anthropic family entry, so ambiguity lands on Unknown.
	if got := visionCapabilityForRouteWithCatalog("", "glm-5.3", cli); got != provider.VisionUnknown {
		t.Fatalf("ambiguous routes = %v, want VisionUnknown", got)
	}

	// (3) A route miss falls through to the family table, and the Bedrock
	// namespace normalization is applied AFTER the catalog tiers.
	if got := visionCapabilityForRouteWithCatalog("", "us.anthropic.claude-sonnet-4-6", cli); got != provider.VisionSupported {
		t.Fatalf("bedrock-normalized model = %v, want VisionSupported", got)
	}
	if got := visionCapabilityForRouteWithCatalog("bedrock-route", "us.anthropic.claude-sonnet-4-5-20250929-v1:0", cli); got != provider.VisionUnsupported {
		t.Fatalf("bedrock route fact = %v, want VisionUnsupported", got)
	}
	if got := visionCapabilityForRouteWithCatalog("", "claude-2.1", cli); got != provider.VisionUnsupported {
		t.Fatalf("text-only lineage = %v, want VisionUnsupported", got)
	}
	if got := visionCapabilityForRouteWithCatalog("", "totally-unknown-model", cli); got != provider.VisionUnknown {
		t.Fatalf("unknown model = %v, want VisionUnknown", got)
	}

	// (4) A nil client must never panic and must still resolve families.
	if got := visionCapabilityForRouteWithCatalog("anything", "claude-opus-4-1-20250805", nil); got != provider.VisionSupported {
		t.Fatalf("nil catalog client = %v, want VisionSupported", got)
	}
}
