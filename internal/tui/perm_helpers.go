package tui

import (
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// permRulesSnapshot converts the live gate's rule list into the
// import-clean PermRule shape the screen package consumes. Mirrors
// renderPermissions's verb-mapping logic.
func (m *Model) permRulesSnapshot() []screen.PermRule {
	rules := m.gate.Snapshot()
	out := make([]screen.PermRule, 0, len(rules))
	for _, r := range rules {
		verb := "ask"
		switch r.Verb {
		case permission.DecisionAllow:
			verb = "allow"
		case permission.DecisionDeny:
			verb = "deny"
		}
		out = append(out, screen.PermRule{
			Verb:   verb,
			Match:  r.Match,
			Source: r.Source,
		})
	}
	return out
}
