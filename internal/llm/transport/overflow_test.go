package transport

import (
	"strings"
	"testing"
)

// TestParseContextOverflow_OpenAICanonical — proves we recover both
// inputTokens and contextLimit from the verbatim DeepSeek/OpenAI 400
// body. This is the body shape that triggered the production Fork
// chain failure (metis test report 2026-05-16 §6 bug #2): the
// parent's 5MB-token history overflowed the 1M cap on Fork warm-spawn.
func TestParseContextOverflow_OpenAICanonical(t *testing.T) {
	body := `{"error":{"message":"This model's maximum context length is 1048576 tokens. However, you requested 1354787 tokens (1346595 in the messages, 8192 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`
	in, cap, ok := ParseContextOverflow(body)
	if !ok {
		t.Fatalf("ParseContextOverflow returned ok=false on canonical OpenAI body")
	}
	if cap != 1048576 {
		t.Errorf("contextLimit = %d; want 1048576", cap)
	}
	// Must pick the "(N in the messages" subgroup, NOT the headline
	// "requested N" — the latter includes the completion budget and
	// would leave the retry under-provisioned.
	if in != 1346595 {
		t.Errorf("inputTokens = %d; want 1346595 (the 'in the messages' subgroup, not the 1354787 headline)", in)
	}
}

func TestParseContextOverflow_OpenAIWithoutMessagesSubgroup(t *testing.T) {
	// Some gateways drop the parenthetical breakdown. Fall back to the
	// headline figure rather than refusing to retry.
	body := `{"error":{"message":"This model's maximum context length is 128000 tokens. However, you requested 145000 tokens. Please reduce."}}`
	in, cap, ok := ParseContextOverflow(body)
	if !ok {
		t.Fatalf("ok=false on simplified OpenAI body")
	}
	if in != 145000 || cap != 128000 {
		t.Errorf("got (%d, %d); want (145000, 128000)", in, cap)
	}
}

func TestParseContextOverflow_AnthropicTooLong(t *testing.T) {
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 215000 tokens > 200000 maximum"}}`
	in, cap, ok := ParseContextOverflow(body)
	if !ok {
		t.Fatalf("ok=false on Anthropic too-long body")
	}
	if in != 215000 || cap != 200000 {
		t.Errorf("got (%d, %d); want (215000, 200000)", in, cap)
	}
}

func TestParseContextOverflow_NotOverflow(t *testing.T) {
	// Other 4xx bodies — rate limits, auth — must return ok=false so
	// the caller doesn't try to "recover" by reducing max_tokens.
	cases := []string{
		`{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`,
		`{"error":{"message":"Invalid API key"}}`,
		``,
		`not even json`,
	}
	for _, body := range cases {
		if _, _, ok := ParseContextOverflow(body); ok {
			t.Errorf("ParseContextOverflow(%q) = ok=true; want false", body)
		}
	}
}

func TestComputeAdjustedMaxTokens_HappyPath(t *testing.T) {
	// 1M cap, 800K input → 199K available after safety buffer → above
	// floor, so we retry with that budget.
	got, ok := ComputeAdjustedMaxTokens(800_000, 1_000_000)
	if !ok {
		t.Fatalf("ok=false; expected room for retry")
	}
	want := 1_000_000 - 800_000 - SafetyBuffer
	if got != want {
		t.Errorf("got %d; want %d", got, want)
	}
}

func TestComputeAdjustedMaxTokens_FloorGuard(t *testing.T) {
	// Input so close to cap that the remaining budget falls below
	// FloorOutputTokens. CC behaviour: don't retry, let the caller
	// surface the hint instead.
	if _, ok := ComputeAdjustedMaxTokens(1_048_000, 1_048_576); ok {
		t.Errorf("ComputeAdjustedMaxTokens should refuse retry when available < FloorOutputTokens")
	}
}

func TestBuildOverflowHint_MentionsFork(t *testing.T) {
	// The hint is consumed by the model on the next turn. If it doesn't
	// surface the Fork→Agent fallback, the model will keep re-spawning
	// Fork against the same parent and loop. The literal "Fork" keyword
	// is what the model latches onto.
	hint := BuildOverflowHint(1_354_787, 1_048_576)
	if !strings.Contains(hint, "Fork") {
		t.Errorf("hint missing 'Fork' keyword; got: %s", hint)
	}
	if !strings.Contains(hint, "1354787") || !strings.Contains(hint, "1048576") {
		t.Errorf("hint missing input/cap figures: %s", hint)
	}
}
