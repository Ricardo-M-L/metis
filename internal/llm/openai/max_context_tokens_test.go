package openai

// max_context_tokens_test.go — pins the per-model context-window
// defaults to vendor-published figures for the OpenAI-compat
// transport. The numbers feed the compaction trigger; a wrong value
// either compacts too late (4xx) or too early (wasted prompt cache).
//
// Source for each prefix lives next to the case in MaxContextTokens —
// update both in lockstep when a vendor publishes a new window.

import "testing"

func TestMaxContextTokens_VendorPublishedDefaults(t *testing.T) {
	// Disable the models.dev background fetch — these tests pin the
	// hardcoded prefix table + suffix parser behaviour. With catalog
	// enabled the background warm-up races in and serves whatever
	// models.dev publishes today, which can drift from the static
	// fixtures here. The Default() singleton checks this env at first
	// call; setting it before the first provider construction is
	// enough because Go's `go test` starts each package fresh.
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	cases := []struct {
		model string
		want  int
	}{
		// OpenAI native
		{"o1-preview", 200_000},
		{"o3-mini", 200_000},
		{"gpt-4o", 128_000},
		{"gpt-4o-mini", 128_000},
		{"gpt-4-turbo", 128_000},
		// OpenAI legacy 32k / 16k variants — caught by suffix tier
		// (32000 / 16000), slightly under the vendor's 32768 / 16385
		// magic numbers. The few-hundred-token gap is harmless;
		// compaction fires very slightly earlier than strictly needed.
		{"gpt-4-32k", 32_000},
		{"gpt-4", 128_000},
		{"gpt-3.5-turbo", 16_385},
		{"gpt-3.5-turbo-16k", 16_000},

		// DeepSeek — the user-asked case
		{"deepseek-v4-pro", 1_000_000},
		{"DeepSeek-V4-Pro", 1_000_000},
		{"deepseek-v3", 128_000},
		{"deepseek-chat", 128_000},
		{"deepseek-coder", 128_000},

		// Kimi / Moonshot
		{"kimi-k2-instruct", 200_000},
		{"Kimi-K2-Chat", 200_000},
		{"moonshot-v1-128k", 128_000}, // tier 4 suffix parsing
		{"moonshot-v1-32k", 32_000},   // tier 4 suffix parsing

		// GLM — Zhipu kept 128K on 4.0-4.5, doubled to 200K on 4.6/4.7/5.x.
		{"glm-4-plus", 128_000},
		{"GLM-4-Plus", 128_000},
		{"glm-4-flash", 128_000},
		{"glm-4.5", 128_000},
		{"glm-4.6", 200_000}, // 2025 bump (per z.ai docs)
		{"glm-4.7", 200_000}, // 2026 release; Flash same window
		{"glm-4.7-flash", 200_000},
		{"glm-5.1", 200_000}, // 2026-04-07 release; docs.bigmodel.cn confirms 200K
		{"GLM-5.1", 200_000},

		// MiniMax via openai_chat (rare but valid)
		{"MiniMax-M2.7", 200_000},

		// Qwen
		{"qwen3-235b-a22b-thinking-2507", 256_000},
		{"qwen2.5-72b", 128_000},
		{"qwen-turbo", 128_000},

		// Mistral
		{"mistral-large-latest", 128_000},
		{"codestral-2501", 256_000},
		{"mistral-medium", 32_000},
		{"mistral-small", 32_000},

		// Llama
		{"llama-3.3-70b", 128_000},
		{"llama-3.1-405b", 128_000},
		{"llama-3-8b", 8_192},
		{"llama-4-maverick", 10_000_000},

		// Suffix tier — unknown prefix but window declared in id
		{"some-self-host-fork-32k", 32_000},
		{"future-model-2m", 2_000_000},

		// Unknown — safe default
		{"completely-novel-model", 128_000},
	}
	for _, c := range cases {
		o := &OpenAI{Model: c.model}
		got := o.MaxContextTokens()
		if got != c.want {
			t.Errorf("MaxContextTokens(%q) = %d; want %d (vendor-published)", c.model, got, c.want)
		}
	}
}

// TestMaxContextTokens_ExplicitOverrideWins — tier 1 always beats
// tier 2/3/4. A hosting gateway capped at 64k must respect the user's
// config.toml regardless of what the model card says.
func TestMaxContextTokens_ExplicitOverrideWins(t *testing.T) {
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	o := &OpenAI{Model: "deepseek-v4-pro", ContextWindow: 64_000}
	if got := o.MaxContextTokens(); got != 64_000 {
		t.Errorf("explicit override should win over prefix table; got %d, want 64000", got)
	}
}
