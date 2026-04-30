package builtin

import (
	"errors"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// applyBashPolicy enforces the [sandbox.bash] allow/deny rules from config.
// Returns nil if the command is allowed, or an error describing why it was
// rejected. Both lists match against the canonical command name (first token,
// with any `.subcmd` suffix stripped — e.g. `mkfs.ext4` → `mkfs`).
//
// allow non-empty:  command must be in allow OR a substring of cmd is in allow
// deny non-empty:   command must NOT be in deny AND no substring of cmd is in deny
//
// Substring matching applies to the full command line so policies like
// `deny = ["rm -rf"]` work without needing to anchor on the command name.
func applyBashPolicy(cmd string, p config.SandboxBashSettings) error {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return errors.New("empty command")
	}
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return errors.New("empty command")
	}
	cmdName := normalizeCommand(parts[0])

	for _, d := range p.Deny {
		if d == "" {
			continue
		}
		if d == cmdName {
			return errors.New("blocked by sandbox.bash.deny: " + d)
		}
		if strings.Contains(cmd, d) {
			return errors.New("blocked by sandbox.bash.deny pattern: " + d)
		}
	}

	if len(p.Allow) > 0 {
		matched := false
		for _, a := range p.Allow {
			if a == "" {
				continue
			}
			if a == cmdName || strings.Contains(cmd, a) {
				matched = true
				break
			}
		}
		if !matched {
			return errors.New("not in sandbox.bash.allow: " + cmdName)
		}
	}
	return nil
}

// applyBashNetworkPolicy mutates a child process env slice based on the
// network setting. Only the `block` mode does anything — it injects bogus
// proxy values that make outbound curl/wget connections fail fast.
//
// This is NOT a real network sandbox; it's a "make casual exfiltration painful"
// guardrail. A determined adversary can clear HTTP_PROXY in their own command.
// For real isolation, use OS-level tools (Docker, unshare) outside Metis.
func applyBashNetworkPolicy(env []string, p config.SandboxBashSettings) []string {
	if p.DangerouslyAllowNetwork {
		return env
	}
	if !strings.EqualFold(p.Network, "block") {
		return env
	}
	const blocked = "http://localhost:0"
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		name := strings.ToUpper(kv[:eq])
		switch name {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			continue // drop, we'll re-add below
		}
		out = append(out, kv)
	}
	out = append(out,
		"HTTP_PROXY="+blocked,
		"HTTPS_PROXY="+blocked,
		"ALL_PROXY="+blocked,
		"NO_PROXY=",
	)
	return out
}
