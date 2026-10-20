package runtime

import "github.com/Ricardo-M-L/metis/internal/principal"

// SnapshotAgentProfile makes an independent value suitable for a durable task.
// Trust provenance survives the round trip, so restoring a project-defined
// role still passes through the ordinary workspace trust gate.
func SnapshotAgentProfile(p *AgentProfile) principal.Profile {
	if p == nil {
		return principal.Profile{}
	}
	return principal.Profile{Name: p.Name, Description: p.Description, Model: p.Model, Tools: append([]string(nil), p.Tools...), DisallowedTools: append([]string(nil), p.DisallowedTools...), PermissionMode: p.PermissionMode, Effort: p.Effort, MaxTurns: p.MaxTurns, InitialPrompt: p.InitialPrompt, Skills: append([]string(nil), p.Skills...), OmitClaudeMd: p.OmitClaudeMd, SystemPrompt: p.SystemPrompt, Source: string(p.Source)}
}

func AgentProfileFromSnapshot(p principal.Profile) *AgentProfile {
	return &AgentProfile{Name: p.Name, Description: p.Description, Model: p.Model, Tools: append([]string(nil), p.Tools...), DisallowedTools: append([]string(nil), p.DisallowedTools...), PermissionMode: p.PermissionMode, Effort: p.Effort, MaxTurns: p.MaxTurns, InitialPrompt: p.InitialPrompt, Skills: append([]string(nil), p.Skills...), OmitClaudeMd: p.OmitClaudeMd, SystemPrompt: p.SystemPrompt, Source: AgentProfileSource(p.Source)}
}
