package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestFormatTurnErrorIncompleteIsNotProviderFailure(t *testing.T) {
	for _, reason := range []string{
		"max_iterations", "diminishing_returns", "loop_detected", "stuck_after_reset",
		"turn_wall_clock", "budget_usd", "acceptance_incomplete", "environment_blocked",
		"max_tokens", "content_filter", "provider_incomplete",
	} {
		t.Run(reason, func(t *testing.T) {
			err := fmt.Errorf("turn ended: %w", &agent.IncompleteRunError{Reason: reason})
			formatted, hint := formatTurnError(err)
			if !strings.HasPrefix(formatted, "Task incomplete:") || !strings.Contains(formatted, reason) {
				t.Fatalf("formatted = %q, want incomplete terminal reason %q", formatted, reason)
			}
			if strings.Contains(formatted, "API Error:") || hint == "" {
				t.Fatalf("incomplete task mislabeled or missing recovery hint: %q, %q", formatted, hint)
			}
		})
	}
}

func TestFormatTurnErrorKeepsProviderAndMemoryFailuresDistinct(t *testing.T) {
	formatted, hint := formatTurnError(errors.New(`openai 429: {"error":{"message":"Rate limit reached","type":"rate_limit_exceeded"}}`))
	if formatted != "API Error: Rate limit reached (rate_limit_exceeded)" || hint == "" {
		t.Fatalf("provider error = %q, %q", formatted, hint)
	}
	formatted, hint = formatTurnError(fmt.Errorf("turn ended: %w", &memory.RecallPersistenceError{Err: errors.New("private storage detail")}))
	if !strings.HasPrefix(formatted, "Memory save error:") || strings.Contains(formatted, "private storage detail") || hint == "" {
		t.Fatalf("memory error = %q, %q", formatted, hint)
	}
	if formatted, hint = formatTurnError(nil); formatted != "" || hint != "" {
		t.Fatalf("nil error = %q, %q", formatted, hint)
	}
}

func TestFinalizeTurnIncompleteRendersRecoveryHintOnce(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("streamed_%t", streamed), func(t *testing.T) {
			m, _ := newSessionSwitchModel(t, permission.ModeAsk)
			err := &agent.IncompleteRunError{Reason: "max_iterations"}
			if streamed {
				m.handleAgentEvent(agent.Event{Kind: agent.EventError, Err: err})
			}
			m.finalizeTurn(err)
			errorsShown, hintsShown := 0, 0
			for _, message := range m.messages {
				switch message.Role {
				case "error":
					errorsShown++
					if message.Content != "Task incomplete: max_iterations" {
						t.Errorf("error row = %q", message.Content)
					}
				case "error-hint":
					hintsShown++
					if !strings.Contains(message.Content, "Send a follow-up") {
						t.Errorf("recovery hint = %q", message.Content)
					}
				}
			}
			if errorsShown != 1 || hintsShown != 1 {
				t.Fatalf("rendered %d errors and %d hints, want one each", errorsShown, hintsShown)
			}
		})
	}
}

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
		// OpenAI-variant — was missing from the old hand-rolled list
		{"exceeds context length: please reduce the length", true},
		// Kimi K2.6 (Moonshot) format
		{"Invalid request: Your request exceeded model token limit: 262144 (requested: 410013) (invalid_request_error)", true},
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
