package openai

import "testing"

func TestSupportsVision(t *testing.T) {
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
		{"o3-mini", true},
		{"o4-mini", true},
		// Text-only / non-vision text-completion lineage
		{"gpt-3.5-turbo", false},
		{"gpt-4", false}, // bare gpt-4 was text-only on launch
		{"text-davinci-003", false},
		// Chinese-OSS vision flagships routed via openai_chat. The
		// list mirrors openai.go::SupportsVision exactly — kept in
		// sync by hand because there's no source-of-truth registry.
		// glm-5.1 + minimax-m2.7 (anthropic side) were live-confirmed
		// 2026-05-20 against /chat/completions with a real PNG.
		{"deepseek-vl-7b", true},
		{"kimi-k2.6", true},
		{"kimi-latest", true},
		{"kimi-vl-thinking", true},
		{"moonshot-v1-vision-preview", true},
		{"glm-5.1", true},
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
		{"glm-4-flash", false},
		// minimax-m* routes via the anthropic transport, not openai — keep false here.
		{"minimax-m2.7", false},
		{"", false},
	}
	for _, c := range cases {
		o := &OpenAI{Model: c.model}
		if got := o.SupportsVision(); got != c.want {
			t.Errorf("SupportsVision(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}
