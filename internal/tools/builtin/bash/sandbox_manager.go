package bash

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

// wrapCommand applies the one runtime-owned sandbox policy to cmd. A nil
// Manager is accepted only for the historical mode=off constructor; an
// enabled configured mode without a Manager is an explicit fail-closed error.
func (b Bash) wrapCommand(cmd *exec.Cmd) (*exec.Cmd, error) {
	if b.sandboxInitErr != nil {
		return nil, b.sandboxInitErr
	}
	if b.sandbox == nil {
		mode, err := sandbox.ParseMode(b.settings.Sandbox.Mode)
		if err != nil {
			return nil, err
		}
		if mode != sandbox.ModeOff {
			return nil, fmt.Errorf("sandbox mode %q requires a runtime sandbox manager", mode)
		}
		return cmd, nil
	}
	return b.sandbox.Wrap(cmd, sandbox.Request{
		Cwd:     cmd.Dir,
		Network: b.sandboxNetworkPolicy(),
	})
}

func (b Bash) sandboxNetworkPolicy() sandbox.NetworkPolicy {
	if !b.settings.Sandbox.DangerouslyAllowNetwork && strings.EqualFold(b.settings.Sandbox.Network, "block") {
		return sandbox.NetworkBlock
	}
	return sandbox.NetworkAllow
}

// commandEnv retains the existing credential filter and soft proxy guard,
// then points every conventional temporary-directory variable at the
// Manager's private writable directory while the OS sandbox is enabled.
func (b Bash) commandEnv(parent []string) []string {
	var env []string
	if b.sandbox != nil {
		env = b.sandbox.FilterEnv(parent, b.settings.Sandbox.DangerouslyInheritEnv)
	} else {
		// Legacy constructor compatibility for mode=off embedders that do not
		// own a runtime Manager.
		env = FilterEnv(parent, b.settings.Sandbox.DangerouslyInheritEnv)
	}
	env = ApplyNetworkPolicy(env, b.settings.Sandbox)
	if b.sandbox == nil || b.sandbox.EffectiveMode() == sandbox.ModeOff {
		return env
	}
	tempDir := b.sandbox.TempDir()
	if tempDir == "" {
		// Wrap will report the closed-manager error before spawn. Do not point
		// TMPDIR back at the host's broad temporary directory in the meantime.
		return env
	}
	return setEnvironment(env, map[string]string{
		"TMPDIR": tempDir,
		"TMP":    tempDir,
		"TEMP":   tempDir,
	})
}

func setEnvironment(env []string, values map[string]string) []string {
	out := make([]string, 0, len(env)+len(values))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, replace := values[strings.ToUpper(name)]; replace {
				continue
			}
		}
		out = append(out, entry)
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if value, ok := values[name]; ok {
			out = append(out, name+"="+value)
		}
	}
	return out
}
