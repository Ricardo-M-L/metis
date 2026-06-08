package main

// slash_runner.go — adapter that lets the model invoke user-authored
// slash commands via the SlashCommand tool. Lives in cmd/metis (not
// internal/tools/builtin) to break the import cycle builtin → slash →
// runtime → builtin: builtin defines the SlashRunner interface, this
// concrete type implements it over the live slash.Registry.
//
// Safety: only Custom commands (loaded from ~/.metis/commands/*.md) are
// runnable. Built-in commands return control Signals the agent loop
// can't honor as a tool result, so they're refused with a clear error.

import (
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/slash"
)

type slashModelRunner struct{ reg *slash.Registry }

// Names lists the invokable (custom) command names for the tool blurb.
func (a slashModelRunner) Names() []string {
	var out []string
	for _, c := range a.reg.All() {
		if c.Custom {
			out = append(out, c.Name)
		}
	}
	return out
}

// RunForModel parses "/name args" (leading slash optional), looks the
// command up, refuses non-custom commands, and returns the expanded body.
func (a slashModelRunner) RunForModel(command string) (string, error) {
	command = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(command), "/"))
	name, args := command, ""
	if i := strings.IndexByte(command, ' '); i >= 0 {
		name, args = command[:i], strings.TrimSpace(command[i+1:])
	}
	if name == "" {
		return "", fmt.Errorf("empty command")
	}
	c, ok := a.reg.Get(name)
	if !ok {
		return "", fmt.Errorf("unknown command %q — use SlashCommand only for user-defined recipes in ~/.metis/commands/", name)
	}
	if !c.Custom {
		return "", fmt.Errorf("%q is a built-in command and can't be invoked as a tool; only user-defined custom commands are", name)
	}
	display, _ := c.Handler(args)
	return display, nil
}
