package runtime

import (
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

// BuildPermissionGate returns a fully wired permission.Gate seeded with the
// allow / deny rules from config.toml.
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
	gate := permission.New(permission.Mode(mode))
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
	return gate
}
