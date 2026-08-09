package sandbox

import "strings"

var blockedEnvFragments = []string{
	"API_KEY", "APIKEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD",
	"PRIVATE_KEY", "AUTH", "CREDENTIALS", "BEARER", "OAUTH",
}

var blockedEnvPrefixes = []string{
	"METIS_", "ANTHROPIC_", "OPENAI_", "MINIMAX_", "GOOGLE_API_",
	"DEEPSEEK_", "GROQ_", "TOGETHER_", "MISTRAL_", "PERPLEXITY_",
	"COHERE_", "OPENROUTER_", "AWS_",
}

// FilterEnv removes credentials from a model-controlled child process and
// points temporary-file users at this Manager's private writable directory.
// Callers that explicitly opt into inheriting secrets may set inheritSecrets;
// TMPDIR and non-interactive agent markers are still normalized.
func (m *Manager) FilterEnv(environ []string, inheritSecrets bool) []string {
	out := make([]string, 0, len(environ)+4)
	for _, kv := range environ {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		name := strings.ToUpper(kv[:eq])
		if !inheritSecrets && blockedEnvName(name) {
			continue
		}
		out = append(out, kv)
	}
	out = replaceEnv(out, "AGENT", "metis")
	out = replaceEnv(out, "AI_AGENT", "metis")
	out = replaceEnv(out, "METIS", "1")
	if temp := m.TempDir(); temp != "" {
		out = replaceEnv(out, "TMPDIR", temp)
	}
	return out
}

func blockedEnvName(name string) bool {
	for _, prefix := range blockedEnvPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	for _, fragment := range blockedEnvFragments {
		if strings.Contains(name, fragment) {
			return true
		}
	}
	return false
}

func replaceEnv(env []string, name, value string) []string {
	prefix := name + "="
	for i := range env {
		if strings.HasPrefix(env[i], prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
