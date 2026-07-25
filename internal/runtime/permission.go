package runtime

import (
	"fmt"
	"os"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

// BuildPermissionGate returns a fully wired permission.Gate seeded with the
// allow / deny rules from config.toml plus the managed policy file.
//
// Extracted from cmd/metis/main.go's setupRuntime so main.go can stay
// composer-only — the contract is "give me cfg + a mode, hand me back a
// gate ready to evaluate." Identity / source-string conventions match
// what the previous inline loop produced ("config:allow" / "config:deny")
// so resumed sessions still match against the same rule provenance.
//
// `mode` is passed separately rather than reading cfg.Permission.Mode
// because callers (resume, --mode flag) can override.
func BuildPermissionGate(cfg *config.Config, mode string) *permission.Gate {
	if mode == "" {
		mode = cfg.Permission.Mode
	}
	gate := permission.New(permission.CanonicalMode(mode))

	// Append order is mostly cosmetic since 2026-06-11: the gate now
	// resolves conflicts by source AUTHORITY first (policy > cli >
	// interactive > config > persistent — see permission/source_rank.go)
	// and append recency only breaks same-authority ties. We still load
	// persistent first / config second so same-rank behavior matches
	// the historical "config overrides persistent" stack.
	//
	// Persistent approvals load from ~/.metis/persistent-permissions.jsonl.
	// Errors are silent — missing / corrupt file just means "no
	// persisted approvals" rather than refusing to start.
	_ = gate.LoadInto(permission.Default(config.Home()))

	for _, r := range cfg.Permission.Allow {
		gate.AppendRules(permission.Rule{
			Tool: r.Tool, Match: r.Match,
			Verb: permission.DecisionAllow, Source: "config:allow",
		})
	}
	for _, r := range cfg.Permission.Deny {
		gate.AppendRules(permission.Rule{
			Tool: r.Tool, Match: r.Match,
			Verb: permission.DecisionDeny, Source: "config:deny",
		})
	}

	// Managed policy (/etc/metis/policy.toml or METIS_POLICY_FILE) —
	// claude-code's policySettings tier. The "policy:" source prefix
	// maps to the top authority rank, so nothing later in the session
	// (config edits, TUI "always allow") can override these rules.
	// A malformed policy file is logged-and-skipped rather than fatal:
	// refusing to boot would lock the user out of fixing it, but the
	// parse error lands on stderr so it isn't silent.
	if pol, err := config.LoadPolicy(); err != nil {
		fmt.Fprintf(os.Stderr, "metis: %v (policy rules NOT applied)\n", err)
	} else if pol != nil {
		for _, r := range pol.Permission.Allow {
			gate.AppendRules(permission.Rule{
				Tool: r.Tool, Match: r.Match,
				Verb: permission.DecisionAllow, Source: "policy:allow",
			})
		}
		for _, r := range pol.Permission.Deny {
			gate.AppendRules(permission.Rule{
				Tool: r.Tool, Match: r.Match,
				Verb: permission.DecisionDeny, Source: "policy:deny",
			})
		}
	}
	return gate
}
