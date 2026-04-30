package builtin

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

func TestApplyBashPolicy_AllowEmpty(t *testing.T) {
	p := config.SandboxBashSettings{}
	if err := applyBashPolicy("ls -la", p); err != nil {
		t.Errorf("empty policy should allow anything, got %v", err)
	}
}

func TestApplyBashPolicy_DenyByName(t *testing.T) {
	p := config.SandboxBashSettings{Deny: []string{"curl"}}
	if err := applyBashPolicy("curl https://example.com", p); err == nil {
		t.Error("deny=[curl] should reject curl")
	}
	if err := applyBashPolicy("ls", p); err != nil {
		t.Errorf("non-denied command should pass; got %v", err)
	}
}

func TestApplyBashPolicy_DenyBySubstring(t *testing.T) {
	p := config.SandboxBashSettings{Deny: []string{"rm -rf"}}
	if err := applyBashPolicy("rm -rf /tmp/x", p); err == nil {
		t.Error("deny substring should reject")
	}
	if err := applyBashPolicy("rm /tmp/x", p); err != nil {
		t.Errorf("partial match should not reject: %v", err)
	}
}

func TestApplyBashPolicy_AllowOnly(t *testing.T) {
	p := config.SandboxBashSettings{Allow: []string{"ls", "cat", "go"}}
	if err := applyBashPolicy("ls -la", p); err != nil {
		t.Errorf("ls should pass: %v", err)
	}
	if err := applyBashPolicy("bash -c 'echo'", p); err == nil {
		t.Error("bash not in allow should reject")
	}
}

func TestApplyBashPolicy_NormalizesSubcommand(t *testing.T) {
	// mkfs.ext4 should match deny=["mkfs"] via canonical name.
	p := config.SandboxBashSettings{Deny: []string{"mkfs"}}
	if err := applyBashPolicy("mkfs.ext4 /dev/sda1", p); err == nil {
		t.Error("mkfs.ext4 should be denied via canonical name 'mkfs'")
	}
}

func TestApplyBashNetworkPolicy_BlockInjectsProxy(t *testing.T) {
	in := []string{"PATH=/usr/bin", "HOME=/x"}
	p := config.SandboxBashSettings{Network: "block"}
	got := applyBashNetworkPolicy(in, p)
	hasProxy := false
	for _, kv := range got {
		if strings.HasPrefix(kv, "HTTP_PROXY=http://localhost:0") {
			hasProxy = true
		}
	}
	if !hasProxy {
		t.Errorf("network=block should inject HTTP_PROXY; got %v", got)
	}
}

func TestApplyBashNetworkPolicy_DangerouslyAllowSkipsBlock(t *testing.T) {
	in := []string{"PATH=/usr/bin"}
	p := config.SandboxBashSettings{Network: "block", DangerouslyAllowNetwork: true}
	got := applyBashNetworkPolicy(in, p)
	for _, kv := range got {
		if strings.HasPrefix(kv, "HTTP_PROXY=") {
			t.Errorf("dangerously_allow_network should not inject proxy; got %v", got)
		}
	}
}

func TestApplyBashNetworkPolicy_DefaultPassthrough(t *testing.T) {
	in := []string{"PATH=/usr/bin", "HTTP_PROXY=http://corp:8080"}
	p := config.SandboxBashSettings{Network: "default"}
	got := applyBashNetworkPolicy(in, p)
	if len(got) != len(in) {
		t.Errorf("default mode should not add/remove env: %v", got)
	}
}
