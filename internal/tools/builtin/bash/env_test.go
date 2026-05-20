package bash

import (
	"strings"
	"testing"
)

func TestFilterEnv_StripsAPIKeys(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"HOME=/home/x",
		"MINIMAX_API_KEY=secret-mm",
		"OPENAI_API_KEY=sk-abc",
		"ANTHROPIC_TOKEN=tok",
		"AWS_ACCESS_KEY_ID=AKIA",
		"AWS_SECRET_ACCESS_KEY=zzz",
		"GH_TOKEN=ghp_xxx",
		"METIS_DEBUG=1",
		"USER_PASSWORD=hunter2",
		"MY_OAUTH_BEARER=tok",
	}
	got := FilterEnv(in, false)
	for _, kv := range got {
		for _, banned := range []string{"API_KEY", "TOKEN", "SECRET", "PASSWORD", "METIS_", "OAUTH"} {
			if strings.Contains(strings.ToUpper(kv), banned) {
				t.Errorf("env leaked sensitive var: %q (matched %q)", kv, banned)
			}
		}
	}
	hasPath, hasHome := false, false
	for _, kv := range got {
		if strings.HasPrefix(kv, "PATH=") {
			hasPath = true
		}
		if strings.HasPrefix(kv, "HOME=") {
			hasHome = true
		}
	}
	if !hasPath || !hasHome {
		t.Errorf("benign env stripped; got %v", got)
	}
}

func TestFilterEnv_DangerouslyInherit(t *testing.T) {
	in := []string{"MINIMAX_API_KEY=secret"}
	got := FilterEnv(in, true)
	// Even under dangerouslyInherit=true the AGENT/AI_AGENT/METIS
	// markers are appended (they're informational, not gating).
	if !containsKV(got, "MINIMAX_API_KEY=secret") {
		t.Errorf("dangerouslyInherit=true should pass the input through; got %v", got)
	}
	for _, want := range []string{"AGENT=metis", "AI_AGENT=metis", "METIS=1"} {
		if !containsKV(got, want) {
			t.Errorf("expected marker %q to be set; got %v", want, got)
		}
	}
}

// TestFilterEnv_AlwaysSetsAgentMarkers — crush-style markers
// (AGENT / AI_AGENT / METIS) must be present in every spawned shell
// so dotfiles and Makefiles can detect "I'm being run by an agent"
// and skip interactive prompts (`gh auth login`'s pager, etc).
func TestFilterEnv_AlwaysSetsAgentMarkers(t *testing.T) {
	got := FilterEnv([]string{"PATH=/usr/bin"}, false)
	for _, want := range []string{"AGENT=metis", "AI_AGENT=metis", "METIS=1"} {
		if !containsKV(got, want) {
			t.Errorf("expected marker %q in output; got %v", want, got)
		}
	}
}

// TestFilterEnv_AgentMarkerOverridesUserSet — a user who happens to
// have `AGENT=otheragent` exported in their shell still gets
// `AGENT=metis` in the spawned bash; we don't want a stale marker
// from a previous tool to mislead our own dotfile rules.
func TestFilterEnv_AgentMarkerOverridesUserSet(t *testing.T) {
	in := []string{"PATH=/usr/bin", "AGENT=other", "AI_AGENT=other"}
	got := FilterEnv(in, false)
	if !containsKV(got, "AGENT=metis") {
		t.Errorf("AGENT should be forced to metis; got %v", got)
	}
	if !containsKV(got, "AI_AGENT=metis") {
		t.Errorf("AI_AGENT should be forced to metis; got %v", got)
	}
	// Make sure we replaced rather than duplicated.
	count := 0
	for _, kv := range got {
		if strings.HasPrefix(kv, "AGENT=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one AGENT= entry; got %d in %v", count, got)
	}
}

func containsKV(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

func TestIsBlockedEnv_CaseInsensitive(t *testing.T) {
	cases := []struct {
		name    string
		blocked bool
	}{
		{"minimax_api_key", true},
		{"My_Secret_Var", true},
		{"PATH", false},
		{"HOME", false},
		{"USER", false},
		{"aws_access_key_id", true},
		{"METIS_SOMETHING", true},
		{"MY_PASSWORD", true},
		{"MY_PRIVATE_KEY", true},
	}
	for _, c := range cases {
		if got := isBlockedEnv(c.name); got != c.blocked {
			t.Errorf("isBlockedEnv(%q) = %v, want %v", c.name, got, c.blocked)
		}
	}
}
