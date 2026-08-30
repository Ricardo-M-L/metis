package mcp

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
	"fmt"
	"os"
	"regexp"
	"sort"
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
// — env-baked values stay out of mcp.toml on the next Save. Missing
// variables collect deduped across all fields.
func expandEnvVarsInEntry(e ServerEntry) (expanded ServerEntry, missing []string) {
	seen := map[string]struct{}{}
	addMissing := func(vars []string) {
		for _, v := range vars {
			if _, dup := seen[v]; !dup {
				seen[v] = struct{}{}
				missing = append(missing, v)
			}
		}
	}
	expanded = ServerEntry{
		Name:     e.Name,
		Disabled: e.Disabled,
	}
	if e.WorkingDir != "" {
		v, m := expandEnvVarsInString(e.WorkingDir)
		expanded.WorkingDir = v
		addMissing(m)
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
	if len(e.Env) > 0 {
		expanded.Env = make(map[string]string, len(e.Env))
		for k, ev := range e.Env {
			v, m := expandEnvVarsInString(ev)
			expanded.Env[k] = v
			addMissing(m)
		}
	}
	expanded.EnabledTools = append([]string(nil), e.EnabledTools...)
	expanded.DisabledTools = append([]string(nil), e.DisabledTools...)
	expanded.Auth = e.Auth
	return expanded, missing
}

// ExpandServerEntry resolves all supported ${VAR} and ${VAR:-default}
// references in an MCP entry without mutating the persisted configuration.
// Every operation that consumes an entry outside LaunchServer (for example an
// explicit OAuth login) should use this helper so it observes the same URL and
// the same clear missing-variable failure as an actual runtime launch.
func ExpandServerEntry(e ServerEntry) (ServerEntry, error) {
	expanded, missing := expandEnvVarsInEntry(e)
	if len(missing) > 0 {
		return ServerEntry{}, fmt.Errorf("mcp: server %q references unset env vars: %s "+
			"(use ${VAR:-default} for an inline fallback)",
			e.Name, strings.Join(missing, ", "))
	}
	return expanded, nil
}

// maybeInjectCUEnv returns the environment and dedicated launch capability for
// Metis's reserved Computer Use server. Both the reserved name and pinned bare
// command must match exactly: an ordinary MCP named by the user cannot gain
// desktop access merely by naming its executable metis-cu, and an arbitrary
// path whose basename is metis-cu is not trusted as the built-in binary.
//
// Background. metis-cu defaults Terminal / iTerm2 / VSCode etc. to
// TierClick "so a stray `type` command can't run a destructive shell."
// That's the right default for general MCP clients, but the metis
// case is structurally different: metis itself runs INSIDE a terminal,
// so the frontmost-app gate trips on every cu launch attempt
// (session 41040bea, 2026-05-26: "tier click on iTerm2 does not
// permit full operations"). Setting the env at spawn time flips the
// gate without touching the user's ~/.metis-cu/config.toml.
func maybeInjectCUEnv(name, command string, env map[string]string) (map[string]string, bool) {
	if name != ReservedComputerUseName || command != ReservedComputerUseBinary {
		return env, false
	}
	out := make(map[string]string, len(env)+3)
	for k, v := range env {
		out[k] = v
	}
	if _, set := out["METIS_CU_HOST_TERMINAL_TIER"]; !set {
		out["METIS_CU_HOST_TERMINAL_TIER"] = "full"
	}
	// metis-cu's Linux backend speaks X11. It needs only the display selector
	// and Xauthority cookie; Wayland and session-bus capabilities remain absent.
	for _, key := range []string{"DISPLAY", "XAUTHORITY"} {
		if _, set := out[key]; set {
			continue
		}
		if value, ok := os.LookupEnv(key); ok && value != "" {
			out[key] = value
		}
	}
	return out, true
}

// envSliceFromMap renders {"K":"V","A":"B"} as ["A=B","K=V"] sorted by
// key so test assertions and debug dumps stay stable across runs.
// Returns nil for an empty/nil map so callers can pass it straight to
// exec.Cmd.Env-aware helpers without an extra nil check.
func envSliceFromMap(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}
