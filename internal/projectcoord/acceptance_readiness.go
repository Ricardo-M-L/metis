package projectcoord

import "strings"

// AcceptanceReadiness describes the host configuration contract, not an
// acceptance outcome. Only WorkItem.Acceptance records executed check results.
// Check command bodies are deliberately excluded: they may contain secrets.
type AcceptanceReadiness struct {
	Status            string  `json:"status"`
	RequiredFor       []Phase `json:"required_for,omitempty"`
	ConfigurationHint string  `json:"configuration_hint,omitempty"`
	Summary           string  `json:"summary"`
}

// HostAcceptanceReadiness gives CLI and model-facing status the same early
// configuration guidance without weakening the verification completion gate.
func HostAcceptanceReadiness(run Run) AcceptanceReadiness {
	view := AcceptanceReadiness{
		Status:  "not_required",
		Summary: "This graph has no verification phase requiring host checks; this is not an acceptance result.",
	}
	for _, item := range run.Items {
		if item.Phase == PhaseVerification {
			view.RequiredFor = []Phase{PhaseVerification}
			break
		}
	}
	if len(run.CheckCommands) > 0 {
		view.Status = "configured"
		view.Summary = "Host acceptance commands are configured. Verification must still run and pass them; configuration is not an acceptance result."
	} else if len(view.RequiredFor) > 0 {
		view.Status = "missing"
		view.Summary = "Research may proceed, but verification cannot complete until the host configures trusted acceptance commands. Ask the host to choose the checks; do not invent them or retry completion with PASS text."
		view.ConfigurationHint = "metis coordinator configure " + quoteAcceptanceArgument(run.ID) + " --cwd " + quoteAcceptanceArgument(run.Workspace) + " --check '<HOST_ACCEPTANCE_COMMAND>'"
	}
	return view
}

// The guidance is a POSIX shell template. Quoting paths as data prevents a
// project directory containing spaces, quotes or substitutions becoming code.
func quoteAcceptanceArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
