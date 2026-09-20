package openai

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestFallbackVisionCapability(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-4o", true},
		{"gpt-4o-2024-11-20", true},
		{"gpt-4o-mini", true},
		{"chatgpt-4o-latest", true},
		{"gpt-5-pro", true},
		{"gpt-4.1", true},
		{"gpt-4-turbo", true},
		{"gpt-4-vision-preview", true},
		// o3 is multimodal but its mini sibling is not; the family prefix must
		// not paper over that (see fallbackVisionCapability).
		{"o3", true},
		{"o3-mini", false},
		{"o4-mini", true},
		// Text-only / non-vision text-completion lineage
		{"gpt-3.5-turbo", false},
		{"gpt-4", false}, // bare gpt-4 was text-only on launch
		{"text-davinci-003", false},
		// Chinese-OSS vision flagships routed via openai_chat. The
		// list mirrors openai.go::fallbackVisionCapability — kept in
		// sync by hand because there's no source-of-truth registry.
		// glm-5.1 + minimax-m2.7 (anthropic side) were live-confirmed
		// 2026-05-20 against /chat/completions with a real PNG.
		{"deepseek-vl-7b", true},
		{"kimi-k2.6", true},
		{"kimi-k2.5", true},
		{"kimi-k2.7-code-highspeed", true},
		{"kimi-k2-6", true},
		{"kimi-latest", true},
		{"kimi-vl-thinking", true},
		{"moonshot-v1-vision-preview", true},
		{"glm-5v-turbo", true},
		{"glm-4.6v", true},
		{"glm-4v-plus", true},
		{"glm-4v-flash", true},
		{"qwen-vl-max", true},
		{"qwen2.5-vl-72b", true},
		// Pre-vision / text-only lineage should stay false.
		// 2026-05-20: deepseek-v4-pro moved here after live API
		// returned 400 "unknown variant image_url" — the model name
		// suggests vision but DeepSeek's list-models endpoint shows
		// no vision-capable V4 id yet.
		{"deepseek-v4-pro", false},
		{"deepseek-v4-flash", false},
		{"deepseek-v3", false},
		{"deepseek-chat", false},
		{"ark-code-latest", false},
		{"kimi-k1.5", false},
		// 2026-09-18: the bare k2 generation (4 live catalog routes) is
		// text-only; the old broad "kimi-k2" vision prefix claimed it.
		{"kimi-k2", false},
		{"kimi-k2-thinking", false},
		{"kimi-k2-turbo-preview", false},
		{"kimi-k2-instruct-fast", false},
		// 2026-09-18: catalog reports glm-5.1 text-only on 18/18 routes,
		// so the old vision entry was a false positive.
		{"glm-5.1", false},
		{"glm-5.2", false},
		{"glm-5.3", false},
		{"glm-4-flash", false},
		// minimax-m* routes via the anthropic transport, not openai — keep false here.
		{"minimax-m2.7", false},
		{"", false},
	}
	for _, c := range cases {
		got := fallbackVisionCapability(c.model) == provider.VisionSupported
		if got != c.want {
			t.Errorf("fallbackVisionCapability(%q) supported = %v, want %v", c.model, got, c.want)
		}
	}
}

func TestCatalogFactPrecedesBroadFallback(t *testing.T) {
	cli := catalog.NewStaticClient(catalog.Catalog{
		"fixture": {Models: map[string]catalog.Model{
			"glm-5.1": {Modalities: catalog.Modalities{Input: []string{"text"}}},
		}},
	})
	if got := visionCapabilityForRouteWithCatalog("", "glm-5.1", cli); got != provider.VisionUnsupported {
		t.Fatalf("catalog text-only fact = %v, want VisionUnsupported", got)
	}
	if got := visionCapabilityForRouteWithCatalog("", "glm-5.2", cli); got != provider.VisionUnsupported {
		t.Fatalf("catalog miss should use confirmed text-only fallback; got %v", got)
	}
}

// TestVisionCapabilityIsRouteScoped pins the 2026-09-18 fix: the same wire id
// is re-published by many gateways with different modalities, so the answer
// must come from the route in use rather than from whichever sibling happens
// to declare image support.
func TestVisionCapabilityIsRouteScoped(t *testing.T) {
	cli := catalog.NewStaticClient(catalog.Catalog{
		"text-gateway": {Models: map[string]catalog.Model{
			"deepseek-v4-flash": {Modalities: catalog.Modalities{Input: []string{"text"}}},
		}},
		"vision-gateway": {Models: map[string]catalog.Model{
			"deepseek-v4-flash": {Modalities: catalog.Modalities{Input: []string{"text", "image"}}},
		}},
		"only-route": {Models: map[string]catalog.Model{
			"some-image-model": {Modalities: catalog.Modalities{Input: []string{"text", "image"}}},
		}},
	})

	if got := visionCapabilityForRouteWithCatalog("text-gateway", "deepseek-v4-flash", cli); got != provider.VisionUnsupported {
		t.Fatalf("text-only route = %v, want VisionUnsupported", got)
	}
	if got := visionCapabilityForRouteWithCatalog("vision-gateway", "deepseek-v4-flash", cli); got != provider.VisionSupported {
		t.Fatalf("image route = %v, want VisionSupported", got)
	}
	// Provider-blind callers must not borrow a sibling route's image support.
	// Conflicting routes stay ambiguous and degrade to the conservative family
	// fact — never to "supported", which is the direction that ships image
	// parts to a text-only endpoint.
	if got := visionCapabilityForRouteWithCatalog("", "deepseek-v4-flash", cli); got != provider.VisionUnsupported {
		t.Fatalf("ambiguous routes = %v, want VisionUnsupported (conservative)", got)
	}
	// A provider id the catalog doesn't carry still resolves via the
	// provider-agnostic path.
	if got := visionCapabilityForRouteWithCatalog("not-a-catalog-provider", "some-image-model", cli); got != provider.VisionSupported {
		t.Fatalf("unknown provider, unanimous routes = %v, want VisionSupported", got)
	}
	// Route fact for an unrelated model stays a miss, not a wrong answer.
	if got := visionCapabilityForRouteWithCatalog("text-gateway", "some-image-model", cli); got != provider.VisionSupported {
		t.Fatalf("route miss should fall through to unanimity; got %v", got)
	}
}

func TestSenseNovaVisionFactPrecedesCatalogLookup(t *testing.T) {
	if got := VisionCapabilityForModel("sensenova-6.8-flash-lite"); got != provider.VisionSupported {
		t.Fatalf("SenseNova capability = %v, want VisionSupported", got)
	}
}

func TestVisionCapabilityDistinguishesUnknownFromUnsupported(t *testing.T) {
	if got := VisionCapabilityForModel("brand-new-private-model"); got != provider.VisionUnknown {
		t.Fatalf("unknown model capability = %v, want VisionUnknown", got)
	}
	if got := VisionCapabilityForModel("gpt-3.5-turbo"); got != provider.VisionUnsupported {
		t.Fatalf("known text-only model capability = %v, want VisionUnsupported", got)
	}
}
