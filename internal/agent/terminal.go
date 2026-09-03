package agent

// IncompleteRunError carries a machine-readable provider/loop terminal reason
// across helpers that cannot return the EventLoopDone stream itself.
type IncompleteRunError struct {
	Reason string
}

func (e *IncompleteRunError) Error() string {
	if e == nil || e.Reason == "" {
		return "task incomplete"
	}
	return "task incomplete: " + e.Reason
}

// IsIncompleteStopReason reports terminal loop outcomes that did not establish
// task completion. Loop.Run emits these through EventLoopDone (rather than an
// EventError) so interactive clients can retain and render the transcript;
// headless and nested consumers must still treat them as unsuccessful.
func IsIncompleteStopReason(stopReason string) bool {
	if IsContentFilterStopReason(stopReason) {
		return true
	}
	switch stopReason {
	case "diminishing_returns",
		"max_iterations",
		"loop_detected",
		"stuck_after_reset",
		"turn_wall_clock",
		"budget_usd",
		"max_tokens",
		"length",
		"max_output_tokens",
		"provider_incomplete",
		"language",
		"malformed_function_call",
		"unexpected_tool_call",
		"too_many_tool_calls",
		"no_image",
		"pause_turn",
		"other",
		"empty_final_answer",
		"provider_protocol_error":
		return true
	default:
		return false
	}
}

// IsContentFilterStopReason reports provider terminal reasons that indicate a
// safety/content-policy refusal rather than a successful answer. These remain
// a subset of incomplete outcomes, but CLI callers can preserve their distinct
// content-filter exit code.
func IsContentFilterStopReason(stopReason string) bool {
	switch stopReason {
	case "content_filter",
		"safety",
		"recitation",
		"blocklist",
		"prohibited_content",
		"spii",
		"image_safety",
		"image_prohibited_content",
		"refusal":
		return true
	default:
		return false
	}
}
