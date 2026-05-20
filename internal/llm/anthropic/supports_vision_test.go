package anthropic

import "testing"

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
