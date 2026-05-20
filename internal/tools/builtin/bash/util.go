package bash

// util.go — package-private helpers. mapDecision was originally in
// internal/tools/builtin/util.go shared across all tools; we keep a
// local 9-line copy here rather than depend on the parent package
// (would create import cycle since register.go in the parent imports
// bash.Bash / bash.List / etc).
//
// If a third place needs the same mapping in future, extract to a
// dedicated `internal/tools/permhelper` package both parent and
// child can import.

import (
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func mapDecision(d permission.Decision) tools.Permission {
	switch d {
	case permission.DecisionAllow:
		return tools.PermissionAllow
	case permission.DecisionDeny:
		return tools.PermissionDeny
	default:
		return tools.PermissionAsk
	}
}

func intStr(i int) string { return fmt.Sprintf("%d", i) }

func bytesString(n int) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// New constructs a Bash tool with the given gate and settings. The
// struct fields are unexported so callers can't build it via a
// composite literal from outside this package — this constructor is
// the supported entry point. Returns Bash by VALUE to match the
// pre-split call shape (`Bash{...}`) and the value-receiver methods.
// Jobs registry is wired later by runtime.RegisterBashJobs when the
// agent loop has constructed it.
func New(gate *permission.Gate, settings config.ToolBashSettings) Bash {
	return Bash{
		gate:       gate,
		settings:   settings,
		classifier: NewBashClassifier(),
	}
}
