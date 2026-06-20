package agent

// cron_permission.go — the permission decision for an UNATTENDED cron
// fire. There is no human at a terminal to answer a mid-fire prompt, so
// (mirroring claude-code's headless/background-agent model where
// `shouldAvoidPermissionPrompts` makes the agent decide from rules alone,
// and MiMo-Code's `interactive:false` ask→deny-clean path) the decision
// is made entirely from pre-authorization:
//
//   - dangerous-pattern commands (rm -rf /, fork bombs, …) → DENY, even
//     when the allow-list would match. Pre-authorization can't punch
//     through the hard floor. claude-code-sourcemap / openclaude both
//     fail-closed on these regardless of mode.
//   - tool call matching any job.AllowTools rule → ALLOW (the user
//     authorized it ahead of time via `cron add --allow` / `cron allow`).
//   - anything else → DENY, and the caller records it to the denied store
//     so the user can review (`cron denied <id>`) and extend the list.
//
// This is reached only for the gate's ASK tier: read-only tools were
// already auto-allowed by the gate and never emit a permission request,
// and a non-bypass gate routes everything else (writes / Bash / network)
// here. We re-check dangerous patterns ourselves because the gate only
// runs that pre-filter in bypass mode (gate.go Check), so in the cron
// runtime's default mode a dangerous command would otherwise arrive here
// as a plain ASK.

import "github.com/Ricardo-M-L/metis/internal/permission"

// FlattenToolInput renders a tool input map into the same canonical
// string form the gate sees (Bash's command, Edit's path, …). Exported so
// the cron CLI can preview a denied call without re-deriving the keys.
func FlattenToolInput(in map[string]any) string { return stringifyToolInput(in) }

// EvaluateCronPermission decides whether an unattended fire of job may
// run a tool call. reason is a short machine-ish tag for the audit log
// and denied store ("dangerous_pattern:…", "allow:<rule>", "unauthorized").
func EvaluateCronPermission(job *CronJob, tool string, input map[string]any) (allow bool, reason string) {
	si := stringifyToolInput(input)

	// Hard floor: never run a known-dangerous command, allow-list or not.
	if hit := permission.CheckDangerousPattern(si); hit != nil {
		return false, "dangerous_pattern:" + hit.Reason
	}
	if job == nil {
		return false, "unauthorized"
	}
	for _, raw := range job.AllowTools {
		rt, rc := permission.ParseToolRule(raw)
		if rt != "*" && rt != tool {
			continue
		}
		if permission.MatchesRuleContent(rc, si) {
			return true, "allow:" + raw
		}
	}
	return false, "unauthorized"
}
