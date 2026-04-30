package tui

import "testing"

// TestFormatProviderError_AnthropicJSON checks the Anthropic / MiniMax
// shape from image #62: provider prefix + nested JSON with error.message.
func TestFormatProviderError_AnthropicJSON(t *testing.T) {
	raw := `anthropic 400: {"type":"error","error":{"type":"invalid_request_error","message":"invalid params, context window exceeds limit (2013)"},"request_id":"064170002a0e423f4723c43ce1b96fc9"}`
	msg, hint := formatProviderError(raw)
	want := "invalid params, context window exceeds limit (2013) (invalid_request_error)"
	if msg != want {
		t.Errorf("msg = %q\nwant %q", msg, want)
	}
	if hint == "" {
		t.Error("expected a recovery hint for context-window overflow")
	}
}

// TestFormatProviderError_OpenAIShape covers OpenAI's error envelope.
func TestFormatProviderError_OpenAIShape(t *testing.T) {
	raw := `openai 429: {"error":{"message":"Rate limit reached","type":"rate_limit_exceeded","code":"rate_limit"}}`
	msg, hint := formatProviderError(raw)
	if msg != "Rate limit reached (rate_limit_exceeded)" {
		t.Errorf("msg = %q", msg)
	}
	if hint == "" {
		t.Error("expected rate-limit hint")
	}
}

// TestFormatProviderError_NonJSON falls back to stripping request_id-
// like suffixes from a raw text error.
func TestFormatProviderError_NonJSON(t *testing.T) {
	raw := `dial tcp: lookup api.example.com: no such host, request_id=abc123`
	msg, _ := formatProviderError(raw)
	if msg != "dial tcp: lookup api.example.com: no such host" {
		t.Errorf("msg = %q (request_id should be stripped)", msg)
	}
}

func TestErrorRecoveryHint_Categories(t *testing.T) {
	cases := []struct {
		in       string
		wantHint bool
	}{
		{"context window exceeds limit", true},
		{"prompt is too long", true},
		{"invalid api key", true},
		{"401 Unauthorized", true},
		{"deadline exceeded", true},
		{"connection refused", true},
		{"some random unknown error", false},
	}
	for _, tc := range cases {
		got := errorRecoveryHint(tc.in)
		if (got != "") != tc.wantHint {
			t.Errorf("errorRecoveryHint(%q) = %q, wantHint=%v", tc.in, got, tc.wantHint)
		}
	}
}

func TestExtractDedupeCount(t *testing.T) {
	cases := map[string]int{
		"API Error: foo":         1,
		"API Error: foo (×2)":    2,
		"API Error: foo (×17)":   17,
		"API Error: foo (×0)":    1, // guard against malformed
		"API Error: foo (× bar)": 1,
	}
	for in, want := range cases {
		if got := extractDedupeCount(in); got != want {
			t.Errorf("extractDedupeCount(%q) = %d, want %d", in, got, want)
		}
	}
}
