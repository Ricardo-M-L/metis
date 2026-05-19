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
		{"gpt-4", false},  // bare gpt-4 was text-only on launch
		{"text-davinci-003", false},
		// Compat-base-URL providers using OpenAI struct
		{"deepseek-v4-pro", false},
		{"kimi-k2.6", false},
		{"glm-5.1", false},
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
