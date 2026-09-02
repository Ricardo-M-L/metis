// Package security implements startup security checks inspired by
// OpenClaw's host-env-security audit. The audit is advisory: it surfaces
// configuration risks (weak Bash denylist, hardcoded API keys, missing
// tool schema fields) but never blocks startup unless the caller chooses to.
package security

import (
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarn     Severity = "warn"
	SeverityCritical Severity = "critical"
)

// Finding is one diagnostic from a security check.
type Finding struct {
	Severity Severity
	Code     string // stable identifier, e.g. "BASH_DENYLIST_WEAK"
	Message  string
	Resource string // optional: file path, tool name, env var, etc.
}

// Report aggregates findings.
type Report struct {
	Findings []Finding
}

// HasCritical returns true if any finding is severity=critical.
func (r *Report) HasCritical() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityCritical {
			return true
		}
	}
	return false
}

// Counts returns counts of findings by severity.
func (r *Report) Counts() (critical, warn, info int) {
	for _, f := range r.Findings {
		switch f.Severity {
		case SeverityCritical:
			critical++
		case SeverityWarn:
			warn++
		case SeverityInfo:
			info++
		}
	}
	return
}

// Run executes all configured checks against cfg + reg and returns a Report.
// Pure function: no I/O, no logging.
func Run(cfg *config.Config, reg *tools.Registry) *Report {
	r := &Report{}
	checkBashDenylist(cfg, r)
	checkAPIKeyHardcoded(cfg, r)
	checkPermissionMode(cfg, r)
	checkToolSchemas(reg, r)
	checkMCPServersTrust(cfg, r)
	return r
}

// --- individual checks ------------------------------------------------------

// requiredDenyPatterns lists substrings every bash denylist should reject.
// Each entry must be a substring of *some* element in the user's denylist.
var requiredDenyPatterns = []string{
	"rm -rf /",
	"dd of=/dev",
	":(){", // classic fork bomb prefix
}

func checkBashDenylist(cfg *config.Config, r *Report) {
	deny := cfg.Tools.Bash.Denylist
	for _, pat := range requiredDenyPatterns {
		if !anyContains(deny, pat) {
			r.Findings = append(r.Findings, Finding{
				Severity: SeverityWarn,
				Code:     "BASH_DENYLIST_WEAK",
				Message:  "tools.bash.denylist does not block " + pat,
				Resource: "config.toml [tools.bash]",
			})
		}
	}
}

func anyContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func checkAPIKeyHardcoded(cfg *config.Config, r *Report) {
	if cfg.Provider.Anthropic.APIKey != "" {
		r.Findings = append(r.Findings, Finding{
			Severity: SeverityWarn,
			Code:     "API_KEY_HARDCODED",
			Message:  "anthropic api_key is set directly in config; prefer api_key_env",
			Resource: "config.toml [provider.anthropic]",
		})
	}
	if cfg.Provider.OpenAI.APIKey != "" {
		r.Findings = append(r.Findings, Finding{
			Severity: SeverityWarn,
			Code:     "API_KEY_HARDCODED",
			Message:  "openai api_key is set directly in config; prefer api_key_env",
			Resource: "config.toml [provider.openai]",
		})
	}
}

func checkPermissionMode(cfg *config.Config, r *Report) {
	mode, ok := permission.ParseMode(cfg.Permission.Mode)
	if !ok {
		return
	}
	switch mode {
	case permission.ModeBypassPermissions:
		r.Findings = append(r.Findings, Finding{
			Severity: SeverityCritical,
			Code:     "PERMISSION_BYPASS",
			Message:  "permission mode = bypassPermissions; tools execute without ordinary confirmation",
			Resource: "config.toml [permission]",
		})
	case permission.ModeFullAccess:
		r.Findings = append(r.Findings, Finding{
			Severity: SeverityCritical,
			Code:     "PERMISSION_FULL_ACCESS",
			Message:  "permission mode = fullAccess; approvals and the process sandbox are disabled, so tools run with the host user's access",
			Resource: "config.toml [permission]",
		})
	}
}

func checkToolSchemas(reg *tools.Registry, r *Report) {
	if reg == nil {
		return
	}
	for _, t := range reg.All() {
		schema := t.InputSchema()
		typ, _ := schema["type"].(string)
		if typ != "object" {
			r.Findings = append(r.Findings, Finding{
				Severity: SeverityWarn,
				Code:     "TOOL_SCHEMA_NON_OBJECT",
				Message:  "tool schema top-level type is not 'object' (got " + typ + ")",
				Resource: t.Name(),
			})
		}
	}
}

func checkMCPServersTrust(cfg *config.Config, r *Report) {
	for _, s := range cfg.MCP.Servers {
		if s.Disabled {
			continue
		}
		// Spawning shell-style commands without absolute paths means PATH
		// hijacking can replace the server with arbitrary code.
		if s.Command != "" && !strings.HasPrefix(s.Command, "/") &&
			!strings.HasPrefix(s.Command, "./") && !strings.HasPrefix(s.Command, "../") {
			r.Findings = append(r.Findings, Finding{
				Severity: SeverityInfo,
				Code:     "MCP_SERVER_RELATIVE_COMMAND",
				Message:  "MCP server command is resolved via PATH; consider using an absolute path",
				Resource: "mcp.servers." + s.Name,
			})
		}
	}
}
