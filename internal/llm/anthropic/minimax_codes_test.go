package anthropic

// minimax_codes_test.go pins the table-driven translation of MiniMax
// business-error codes to user-facing hints. The user is on
// MiniMax-M2.7 by default so a wrong mapping shows up in their
// face within minutes.

import (
	"errors"
	"strings"
	"testing"
)

func TestExtractMinimaxHint_KnownCodes(t *testing.T) {
	cases := []struct {
		body       string
		wantSubstr string // substring expected in the hint
	}{
		{`{"message":"insufficient credits (1028)"}`, "quota"},
		{`{"message":"too many requests (1030)"}`, "rate limit"},
		{`{"message":"input rejected (1002)"}`, "content_filter"},
		{`{"message":"output blocked (1039)"}`, "content_filter"},
		{`{"message":"model not on your plan (2061)"}`, "Plus, Video requires Max"},
		{`{"message":"tool args bad (2013)"}`, "invalid tool arguments"},
		{`{"message":"context window exceeds limit (2026)"}`, "context window"},
		{`{"message":"invalid api key (1004)"}`, "auth"},
		{`{"message":"account suspended (1008)"}`, "suspended"},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			got := extractMinimaxHint(c.body)
			if got == "" {
				t.Fatalf("expected hint for body %q; got empty", c.body)
			}
			if !strings.Contains(got, c.wantSubstr) {
				t.Errorf("hint = %q, want substring %q", got, c.wantSubstr)
			}
		})
	}
}

func TestExtractMinimaxHint_UnknownCodeReturnsEmpty(t *testing.T) {
	got := extractMinimaxHint(`{"message":"weird thing (9999)"}`)
	if got != "" {
		t.Errorf("unknown code should return empty hint; got %q", got)
	}
}

func TestExtractMinimaxHint_NoCodeReturnsEmpty(t *testing.T) {
	got := extractMinimaxHint(`{"message":"plain non-MiniMax error"}`)
	if got != "" {
		t.Errorf("body without (NNNN) suffix should return empty; got %q", got)
	}
}

func TestExtractMinimaxHint_EmptyBody(t *testing.T) {
	if got := extractMinimaxHint(""); got != "" {
		t.Errorf("empty body should return empty hint; got %q", got)
	}
}

// TestWrapWithMinimaxHint_PreservesUnwrap — the resulting error must
// still chain back to the original via errors.Is/errors.As. This
// matters because exitcode.Classify and other layered handlers walk
// the chain to find typed sentinels.
func TestWrapWithMinimaxHint_PreservesUnwrap(t *testing.T) {
	original := errors.New("anthropic 429: {\"message\":\"too many (1030)\"}")
	wrapped := wrapWithMinimaxHint(original, `{"message":"too many (1030)"}`)
	if !errors.Is(wrapped, original) {
		t.Errorf("wrapped error should still match the original via errors.Is")
	}
	if !strings.Contains(wrapped.Error(), "rate limit") {
		t.Errorf("wrapped error should embed the friendly hint; got %q", wrapped.Error())
	}
}

func TestWrapWithMinimaxHint_NoOpOnUnknown(t *testing.T) {
	original := errors.New("anthropic 500: random failure")
	wrapped := wrapWithMinimaxHint(original, `{"message":"random"}`)
	if wrapped.Error() != original.Error() {
		t.Errorf("unknown-code wrap should be a no-op; got %q", wrapped.Error())
	}
}

func TestWrapWithMinimaxHint_NilError(t *testing.T) {
	if got := wrapWithMinimaxHint(nil, "(1028)"); got != nil {
		t.Errorf("nil error should remain nil; got %v", got)
	}
}
