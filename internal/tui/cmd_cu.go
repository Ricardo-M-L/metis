package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/computeruse"
)

const cuUsage = "usage: /cu [status|install|enable|disable|stop|permissions-accessibility|permissions-screen-recording]"

// cmdCU delegates all management to the process-owned Computer Use service.
// A disconnected surface must never register or launch a PATH-resolved server.
func cmdCU(r *REPL, args string) string {
	parts := strings.Fields(args)
	if len(parts) > 1 {
		return cuUsage
	}
	action := "status"
	if len(parts) == 1 {
		action = parts[0]
	}
	switch action {
	case "on":
		action = "enable"
	case "off":
		action = "disable"
	case "help", "-h", "--help":
		return cuUsage
	case "status", "install", "enable", "disable", "stop", "permissions-accessibility", "permissions-screen-recording":
	default:
		return fmt.Sprintf("cu: unknown action %q. %s", action, cuUsage)
	}
	if r == nil || r.ComputerUse == nil {
		return "cu: unavailable — Computer Use manager is not connected to this session"
	}
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	status, err := r.ComputerUse(ctx, action)
	if err != nil {
		return "cu: " + err.Error()
	}
	return formatComputerUseStatus(status)
}

func formatComputerUseStatus(status computeruse.Status) string {
	installed, enabled, running := "not installed", "disabled", "stopped"
	if status.Installed {
		installed = "installed"
	}
	if status.Enabled {
		enabled = "enabled"
	}
	if status.Running {
		running = "running"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "cu: %s; %s; %s", installed, enabled, running)
	if status.Version != "" {
		fmt.Fprintf(&out, "\n  version: %s", status.Version)
	}
	if status.Source != "" {
		fmt.Fprintf(&out, "\n  source: %s", status.Source)
	}
	if status.Path != "" {
		fmt.Fprintf(&out, "\n  binary: %s", status.Path)
	}
	if status.Description != nil {
		keys := make([]string, 0, len(status.Description.Permissions))
		for name := range status.Description.Permissions {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			fmt.Fprintf(&out, "\n  %s: %s", name, status.Description.Permissions[name])
		}
	}
	if status.Message != "" {
		fmt.Fprintf(&out, "\n  %s", status.Message)
	}
	return out.String()
}
