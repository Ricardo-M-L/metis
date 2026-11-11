package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
)

func agentRequestedOutputCap(input map[string]any, provider llm.Provider) (int, error) {
	value, present := input["max_output_tokens"]
	if !present {
		return 0, nil
	}
	var number int64
	switch n := value.(type) {
	case int:
		number = int64(n)
	case int64:
		number = n
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n <= 0 || n > float64(math.MaxInt32) {
			return 0, errors.New("max_output_tokens must be a positive integer")
		}
		number = int64(n)
	case json.Number:
		var err error
		number, err = n.Int64()
		if err != nil {
			return 0, errors.New("max_output_tokens must be a positive integer")
		}
	default:
		return 0, errors.New("max_output_tokens must be a positive integer")
	}
	if number <= 0 || number > math.MaxInt32 {
		return 0, errors.New("max_output_tokens must be a positive integer")
	}
	maximum := llm.ProviderDefaultOutputBudget(provider)
	if maximum <= 0 {
		return 0, errors.New("max_output_tokens is not supported by this provider's server-managed or unknown output budget; omit it to inherit")
	}
	if number > int64(maximum) {
		return 0, fmt.Errorf("max_output_tokens may only reduce the selected child provider's output budget (maximum %d)", maximum)
	}
	return int(number), nil
}

// Diagnostics are host-authored classifications, never raw provider HTTP
// bodies, panic payloads, credentials, or model-authored guesses.
func subAgentFailureDetails(snap agent.TeammateSnapshot) (reason, diagnostic string, canResume bool) {
	reason = strings.TrimSpace(snap.StopHint)
	if snap.Status == agent.StatusCompleted || snap.Status.IsActive() {
		return reason, "", false
	}
	var httpError *transport.HTTPStatusError
	switch {
	case reason == "max_tokens" || reason == "length" || reason == "max_output_tokens":
		return reason, "The sub-agent reached its per-response output limit before finishing. Partial output is retained; automatic text-only continuation is bounded and cannot execute tools.", true
	case errors.Is(snap.ExitErr, context.Canceled) || snap.Status == agent.StatusKilled:
		return "cancelled", "The sub-agent was cancelled. Any partial output does not establish task completion.", true
	case errors.Is(snap.ExitErr, context.DeadlineExceeded) || strings.HasPrefix(reason, "timeout ") || reason == "timeout" || reason == "turn_wall_clock":
		deadline := "at its execution deadline"
		if duration, err := time.ParseDuration(strings.TrimPrefix(reason, "timeout ")); err == nil && duration > 0 {
			deadline = "after " + duration.String()
		}
		return "timeout", "The sub-agent timed out " + deadline + ". Partial output is retained; inspect existing effects before explicitly resuming.", true
	case reason == "permission_denied":
		return reason, "The sub-agent was denied by its permission boundary. Existing effects must be inspected before requesting any new authorization.", false
	case reason == "api_error" || errors.As(snap.ExitErr, &httpError) || errors.Is(snap.ExitErr, transport.ErrNetwork):
		return "api_error", "The sub-agent provider request failed. Partial output is retained; automatic output continuation was not attempted.", true
	case reason == "panic":
		return reason, "The sub-agent stopped after an internal panic. Partial output does not establish task completion.", false
	case reason == "provider_protocol_error" || reason == "provider_incomplete":
		return reason, "The provider returned an incomplete or invalid response. Unaccepted tool calls were not executed.", false
	case agent.IsContentFilterStopReason(reason):
		return reason, "The provider stopped the response under its content policy. Automatic continuation was not attempted.", false
	case reason == "max_iterations":
		return reason, "The sub-agent exhausted its execution iteration budget before completing.", true
	case reason == "acceptance_incomplete":
		return reason, "The text-only continuation could not establish task completion without additional work.", true
	case reason == "setup_failed" || reason == "admission_failed":
		return reason, "The sub-agent could not enter its execution boundary.", false
	default:
		if reason == "" {
			reason = "execution_error"
		}
		return reason, "The sub-agent failed during execution. Inspect its saved status and partial output before retrying.", false
	}
}

func subAgentFailureOutput(snap agent.TeammateSnapshot, reason, diagnostic string, canResume bool) string {
	var result strings.Builder
	fmt.Fprintf(&result, "Sub-agent execution incomplete: agent_id=%s name=%q status=%s stop_reason=%s continuation_attempts=%d partial_output=%t can_resume=%t\nDiagnostic: %s", snap.AgentID, snap.Name, snap.Status, reason, snap.ContinuationAttempts, strings.TrimSpace(snap.Output) != "", canResume, diagnostic)
	partial := strings.TrimSpace(snap.Output)
	if partial == "" {
		partial = strings.TrimSpace(snap.Result)
	}
	if partial != "" {
		result.WriteString("\n---partial output (not a completed result)---\n")
		result.WriteString(partial)
	}
	if canResume {
		fmt.Fprintf(&result, "\nInspect SubAgentOutput(agent_id=%q); only the direct parent may explicitly continue this child with Agent(resume_from=%q).", snap.AgentID, snap.AgentID)
	}
	return result.String()
}

func safeSubAgentNotificationError(snap agent.TeammateSnapshot) error {
	if snap.ExitErr == nil {
		return nil
	}
	reason, diagnostic, _ := subAgentFailureDetails(snap)
	return fmt.Errorf("sub-agent stop_reason=%s: %s", reason, diagnostic)
}
