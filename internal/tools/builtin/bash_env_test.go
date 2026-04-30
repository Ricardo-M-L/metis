package builtin

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
	got := filterEnv(in, false)
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
	got := filterEnv(in, true)
	if len(got) != 1 || got[0] != "MINIMAX_API_KEY=secret" {
		t.Errorf("dangerouslyInherit=true should pass through; got %v", got)
	}
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
