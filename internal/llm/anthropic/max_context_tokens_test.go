package anthropic

// max_context_tokens_test.go — pins the per-model context-window
// defaults to vendor-published figures. The numbers drive the
// `33423 tokens (17%)` status-bar percent display + the compaction
// threshold (70% / 85% of this denominator), so a wrong number
// either over-displays utilisation (192k → 17.4% for 33k tokens
// when the true window is 200k → 16.7%) or fires compaction too
// late.
//
// Source for each prefix is in the function-level doc on
// MaxContextTokens. Update there + here in lockstep when a vendor
// publishes new windows.

import "testing"

func TestMaxContextTokens_VendorPublishedDefaults(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		// Anthropic native
		{"claude-opus-4-7", 200000},
		{"claude-sonnet-4-6", 200000},
		{"claude-haiku-4-5", 200000},
		// MiniMax via api.minimaxi.com/anthropic gateway
		{"MiniMax-M2.7", 200000},
		{"MiniMax-M2", 200000},
		{"minimax-text-01", 200000},
		// GLM via zhipuai
		{"GLM-4-Plus", 128000},
		{"glm-4-flash", 128000},
		// DeepSeek
		{"deepseek-chat", 128000},
		{"DeepSeek-V3", 128000},
		// Kimi / Moonshot
		{"kimi-k2-instruct", 200000},
		{"moonshot-v1-128k", 200000},
		// Unknown — falls through to 200k safe default
		{"some-future-model", 200000},
	}
	for _, c := range cases {
		a := &Anthropic{Model: c.model}
		got := a.MaxContextTokens()
		if got != c.want {
			t.Errorf("MaxContextTokens(%q) = %d; want %d (vendor-published)", c.model, got, c.want)
		}
	}
}

func TestMaxContextTokens_ExplicitOverrideWins(t *testing.T) {
	// Hosting gateways sometimes cap below the vendor figure (a hosted
	// MiniMax compat layer running on 64k slices, for example). The
	// `[provider.X].context_window` config override must be respected
	// over the prefix table; otherwise users tune their config and
	// still see the wrong denominator.
	a := &Anthropic{Model: "MiniMax-M2.7", ContextWindow: 64000}
	if got := a.MaxContextTokens(); got != 64000 {
		t.Errorf("explicit ContextWindow override should win; got %d, want 64000", got)
	}
}
