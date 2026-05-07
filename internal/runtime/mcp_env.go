package runtime

// mcp_env.go — environment variable expansion for MCP server entries.
//
// Lifted from Claude Code's services/mcp/envExpansion.ts (`expandEnvVarsInString`).
// The motivation is the same: lets users write
//
//	[[servers]]
//	name = "github"
//	command = "uvx"
//	args = ["github-mcp-server"]
//	[servers.headers]
//	  Authorization = "Bearer ${GITHUB_TOKEN}"
//
// in mcp.toml without committing secrets to disk. ${VAR:-default} is
// supported for fallbacks. Missing-without-default vars surface in the
// returned `missing` slice so callers can warn the user instead of
// silently empty-substituting (otherwise debugging "why is auth failing"
// turns into a guessing game).

import (
	"os"
	"regexp"
	"strings"
)

// envVarPattern matches ${NAME} and ${NAME:-default}. The default value
// is captured separately and passed through verbatim — it can contain
// any character except `}`, including spaces and other ${...} sequences
// (we only expand one level — nested expansion is rare and the rules get
// fuzzy fast).
var envVarPattern = regexp.MustCompile(`\$\{([^}:]+)(?::-([^}]*))?\}`)

// expandEnvVarsInString resolves ${VAR} and ${VAR:-default} against
// os.Getenv. Missing variables (no default) are left as the original
// `${VAR}` literal AND collected into `missing` so callers can warn —
// dropping them silently would produce empty strings ("Bearer " etc)
// that fail authentication at the server with no useful diagnostic.
func expandEnvVarsInString(s string) (expanded string, missing []string) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	seen := map[string]struct{}{}
	out := envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		groups := envVarPattern.FindStringSubmatch(match)
		if len(groups) < 2 {
			return match
		}
		name := groups[1]
		var def string
		hasDef := len(groups) >= 3 && len(groups[0]) > len("${"+name+"}")
		if hasDef {
			def = groups[2]
		}
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		if hasDef {
			return def
		}
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			missing = append(missing, name)
		}
		return match // leave the ${VAR} literal so it's visible in errors
	})
	return out, missing
}

// expandEnvVarsInEntry returns a copy of the entry with ${VAR} expanded
// in command, args, url, and headers. The original entry is NOT mutated
// — env-baked values stay out of mcp.toml on the next SaveMCP. Missing
// variables collect deduped across all fields.
func expandEnvVarsInEntry(e MCPServerEntry) (expanded MCPServerEntry, missing []string) {
	seen := map[string]struct{}{}
	addMissing := func(vars []string) {
		for _, v := range vars {
			if _, dup := seen[v]; !dup {
				seen[v] = struct{}{}
				missing = append(missing, v)
			}
		}
	}
	expanded = MCPServerEntry{
		Name:     e.Name,
		Disabled: e.Disabled,
	}
	if e.Command != "" {
		v, m := expandEnvVarsInString(e.Command)
		expanded.Command = v
		addMissing(m)
	}
	if len(e.Args) > 0 {
		expanded.Args = make([]string, len(e.Args))
		for i, a := range e.Args {
			v, m := expandEnvVarsInString(a)
			expanded.Args[i] = v
			addMissing(m)
		}
	}
	if e.URL != "" {
		v, m := expandEnvVarsInString(e.URL)
		expanded.URL = v
		addMissing(m)
	}
	if len(e.Headers) > 0 {
		expanded.Headers = make(map[string]string, len(e.Headers))
		for k, hv := range e.Headers {
			ev, m := expandEnvVarsInString(hv)
			expanded.Headers[k] = ev
			addMissing(m)
		}
	}
	return expanded, missing
}
