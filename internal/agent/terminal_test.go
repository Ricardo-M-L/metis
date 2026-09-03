package agent

import "testing"

func TestIsIncompleteStopReason(t *testing.T) {
	for _, stopReason := range []string{
		"diminishing_returns", "max_iterations", "loop_detected",
		"stuck_after_reset", "turn_wall_clock", "budget_usd",
		"max_tokens", "length", "max_output_tokens",
		"provider_incomplete", "language", "malformed_function_call",
		"unexpected_tool_call", "too_many_tool_calls", "no_image", "pause_turn", "other",
		"content_filter", "safety", "recitation", "blocklist", "prohibited_content",
		"spii", "image_safety", "image_prohibited_content", "refusal",
		"empty_final_answer", "provider_protocol_error",
	} {
		if !IsIncompleteStopReason(stopReason) {
			t.Errorf("%q should be incomplete", stopReason)
		}
	}
	for _, stopReason := range []string{
		"content_filter", "safety", "recitation", "blocklist", "prohibited_content",
		"spii", "image_safety", "image_prohibited_content", "refusal",
	} {
		if !IsContentFilterStopReason(stopReason) {
			t.Errorf("%q should be a content-filter stop", stopReason)
		}
	}
	for _, stopReason := range []string{"", "end_turn", "plan_mode", "halted_by_hook", "tool_use"} {
		if IsIncompleteStopReason(stopReason) {
			t.Errorf("%q should not be incomplete", stopReason)
		}
		if IsContentFilterStopReason(stopReason) {
			t.Errorf("%q should not be a content-filter stop", stopReason)
		}
	}
}

func TestIncompleteRunError(t *testing.T) {
	if got := (&IncompleteRunError{Reason: "max_tokens"}).Error(); got != "task incomplete: max_tokens" {
		t.Fatalf("error = %q", got)
	}
}
