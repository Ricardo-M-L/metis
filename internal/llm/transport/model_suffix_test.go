package transport

import "testing"

// TestParseModelWindowSuffix_AllVendorConventions — the suffix-parsing
// tier is the safety net for unknown models. It must handle every
// vendor convention seen in the wild without false positives.
func TestParseModelWindowSuffix_AllVendorConventions(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		// k suffix — kilo (1000)
		{"deepseek-v3.2-32k", 32_000, true},
		{"moonshot-v1-128k", 128_000, true},
		{"kimi-k1.5-200k", 200_000, true},
		{"qwen3-235b-a22b-thinking-2507-256k", 256_000, true},
		{"some-fork-1024k", 1_024_000, true},

		// m suffix — million
		{"deepseek-v4-1m", 1_000_000, true},
		{"gemini-1.5-pro-2m", 2_000_000, true},

		// Case-insensitive — vendors aren't consistent.
		{"DEEPSEEK-V4-1M", 1_000_000, true},
		{"Moonshot-V1-128K", 128_000, true},

		// Negative cases — no suffix or non-window k/m inside body.
		{"deepseek-v4-pro", 0, false},       // "pro" isn't a number
		{"claude-opus-4-7", 0, false},       // numeric segment but no k/m
		{"kimi-k2-instruct", 0, false},      // "k2" is family, not "-2k"
		{"gpt-4o-mini", 0, false},           // contains 'o' but no suffix
		{"", 0, false},                      // empty
		{"32k-prefix-not-suffix", 0, false}, // suffix-only rule
		{"-5k", 5_000, true},                // bare "-Nk" still matches by spec — caller filters
	}
	for _, c := range cases {
		got, ok := ParseModelWindowSuffix(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseModelWindowSuffix(%q) = (%d, %v); want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestParseModelWindowSuffix_RejectsZeroAndNegative — defensive:
// `-0k` would parse to 0 tokens which is meaningless; reject so the
// caller falls through to the next tier instead of returning a
// useless zero window.
func TestParseModelWindowSuffix_RejectsZeroAndNegative(t *testing.T) {
	for _, in := range []string{"foo-0k", "bar-0m"} {
		if got, ok := ParseModelWindowSuffix(in); ok || got != 0 {
			t.Errorf("ParseModelWindowSuffix(%q) = (%d, %v); want (0, false)", in, got, ok)
		}
	}
}
