package anthropic

// Unit tests for the MiniMax-style "invalid function arguments json
// string" 400 retry path (anthropic.go's isInvalidToolArgsError +
// withToolArgsReminder). Black-box only — no network — to keep CI
// hermetic across runners.

import (
	"strings"
	"testing"
)

func TestIsInvalidToolArgsError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain 400", makeErr("anthropic 400: bad request"), false},
		{"matches MiniMax 2013", makeErr(`anthropic 400: {"type":"error","error":{"type":"invalid_request_error","message":"invalid params, invalid function arguments json string, tool_call_id: call_function_xyz_1 (2013)"}}`), true},
		{"matches without code", makeErr("invalid function arguments somewhere"), true},
		{"network error unrelated", makeErr("dial tcp: connection refused"), false},
		{"rate limit", makeErr("anthropic 429: rate limited"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isInvalidToolArgsError(tc.err)
			if got != tc.want {
				t.Errorf("isInvalidToolArgsError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestWithToolArgsReminder_AppendsBlock(t *testing.T) {
	original := anthropicReq{
		Model: "claude-test",
		System: []anthropicSystemBlock{
			{Type: "text", Text: "you are a helpful assistant"},
		},
	}

	out := withToolArgsReminder(original)

	// Original must not be mutated (subsequent retries reuse it).
	if len(original.System) != 1 {
		t.Errorf("withToolArgsReminder mutated input: now %d blocks", len(original.System))
	}
	if len(out.System) != 2 {
		t.Fatalf("expected 2 system blocks after reminder, got %d", len(out.System))
	}
	reminder := out.System[1].Text
	if !strings.Contains(reminder, "valid non-empty JSON") {
		t.Errorf("reminder doesn't mention valid JSON: %q", reminder)
	}
	if !strings.Contains(reminder, `"_": ""`) {
		t.Errorf("reminder doesn't mention the `_` field workaround: %q", reminder)
	}
	if !strings.Contains(reminder, "{}") {
		t.Errorf("reminder doesn't mention {} literal: %q", reminder)
	}
}

func TestWithToolArgsReminder_EmptySystem(t *testing.T) {
	original := anthropicReq{Model: "claude-test"}
	out := withToolArgsReminder(original)
	if len(out.System) != 1 {
		t.Fatalf("empty input should produce 1 reminder block, got %d", len(out.System))
	}
	if out.System[0].Type != "text" {
		t.Errorf("reminder type = %q, want text", out.System[0].Type)
	}
}

func TestWithToolArgsReminder_DoesNotAliasSystemSlice(t *testing.T) {
	// Regression: prior bug where the function appended to the caller's
	// slice, mutating it for subsequent retries. Verify by appending
	// twice and checking the original stayed at length 1.
	original := anthropicReq{
		System: []anthropicSystemBlock{{Type: "text", Text: "base"}},
	}
	_ = withToolArgsReminder(original)
	_ = withToolArgsReminder(original)
	if len(original.System) != 1 {
		t.Errorf("original system slice mutated: len=%d, want 1", len(original.System))
	}
}

// makeErr builds a quick error wrapping a string. Local helper so the
// table tests don't pull in errors.New repetitively.
func makeErr(s string) error { return &stringErr{s: s} }

type stringErr struct{ s string }

func (e *stringErr) Error() string { return e.s }
