package permission

// Rule-source precedence — claude-code parity for the
// PermissionRuleSource ladder (restored-src/src/types/permissions.ts:
// 55-91, policySettings > cliArg > ... > userSettings). metis rule
// sources are free-form strings for diagnostics; this table maps their
// stable prefixes onto ranks so the gate can resolve conflicts by
// AUTHORITY first and recency second, instead of pure append order.
//
// Why authority matters: a managed policy deny (`/etc/metis/policy.toml`)
// must not be overridable by the user clicking "always allow" in the
// TUI — the interactive rule appends later (previously: won), but it
// carries less authority.
//
// Ties (same rank) keep the existing last-appended-wins behavior, so
// every current same-source interaction is unchanged.

import "strings"

const (
	// rankPolicy MUST stay the maximum rank: Gate.Check short-circuits
	// its back-to-front scan the moment it sees a policy-rank match
	// (gate.go), relying on nothing being able to outrank it. A new
	// tier above policy would silently break that early-exit — add it
	// BELOW rankPolicy, or update the Check loop's ceiling guard.
	rankPolicy      = 100 // /etc/metis/policy.toml — managed, never overridable
	rankCLI         = 80  // --allow / --deny flags on this invocation
	rankInteractive = 60  // TUI "always allow" during this session
	rankConfig      = 40  // ~/.metis/config.toml + .metis/config{,.local}.toml
	rankPersistent  = 20  // persisted "yes always" approvals from prior sessions
)

func sourceRank(source string) int {
	switch {
	case strings.HasPrefix(source, "policy"):
		return rankPolicy
	case strings.HasPrefix(source, "cli"):
		return rankCLI
	case source == "interactive" || strings.HasPrefix(source, "session"):
		return rankInteractive
	case source == "persistent":
		return rankPersistent
	default:
		// config:* and anything unrecognized (plugins, resume
		// passthrough) sit at config authority — the pre-ranking
		// behavior for the common case.
		return rankConfig
	}
}

// SanitizeResumedSource caps the authority a rule can carry across the
// resume boundary. Session files are user-editable JSON: a forged
// "policy*" or "cli*" source would otherwise resurrect a rule with
// un-overridable rank. Legit policy / CLI rules are rebuilt fresh at
// every boot (BuildPermissionGate / flag parsing), so a resumed copy
// never legitimately needs more than interactive authority.
func SanitizeResumedSource(source string) string {
	if sourceRank(source) > rankInteractive {
		return "session:resumed(" + source + ")"
	}
	return source
}

// ResumedSessionSource marks every rule read from a session file as
// session-scoped, regardless of the diagnostic source stored in that file.
// Sanitizing authority alone is insufficient for a long-lived TUI: a saved
// rule claiming "config:allow" would otherwise survive the next in-process
// session switch because it looks like process-scoped configuration.
func ResumedSessionSource(source string) string {
	source = SanitizeResumedSource(source)
	if source == "" {
		source = "unknown"
	}
	if strings.HasPrefix(source, "session:") {
		return source
	}
	return "session:resumed(" + source + ")"
}
