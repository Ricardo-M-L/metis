package mcp

import (
	"context"
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/computeruse"
	"github.com/Ricardo-M-L/metis/internal/config"
)

// ManagedComputerUseCommand is a host-resolved component reference, never a
// PATH lookup. Its executable must pass the private installation's hash and
// protocol validation before receiving desktop sandbox capabilities.
const ManagedComputerUseCommand = "@metis/computer-use"

func SetManagedComputerUseServer(reg *Registry) error {
	if reg == nil {
		return fmt.Errorf("managed Computer Use: missing registry")
	}
	if entry := FindServer(reg, ReservedComputerUseName); entry != nil {
		if entry.Command != ManagedComputerUseCommand || entry.URL != "" {
			return fmt.Errorf("managed Computer Use: existing custom server was not replaced; remove it explicitly before enabling the managed component")
		}
		if err := validateManagedComputerUseEntry(*entry); err != nil {
			return err
		}
		entry.Disabled = false
		return nil
	}
	reg.Servers = append(reg.Servers, ServerEntry{Name: ReservedComputerUseName, Command: ManagedComputerUseCommand})
	return nil
}

func validateManagedComputerUseEntry(entry ServerEntry) error {
	if entry.Name != ReservedComputerUseName || entry.URL != "" || len(entry.Args) != 0 || len(entry.Env) != 0 || len(entry.Headers) != 0 || entry.Auth != "" || entry.WorkingDir != "" {
		return fmt.Errorf("managed Computer Use: custom transport, arguments and environment overrides are not allowed")
	}
	return nil
}

func prepareManagedComputerUseEntry(ctx context.Context, entry ServerEntry) (ServerEntry, error) {
	if entry.Command != ManagedComputerUseCommand {
		return entry, nil
	}
	if err := validateManagedComputerUseEntry(entry); err != nil {
		return ServerEntry{}, err
	}
	home, err := config.VerifiedHome()
	if err != nil {
		return ServerEntry{}, err
	}
	// Startup reaches this only for an enabled managed entry. Preserve that
	// opt-in across METIS updates by adopting/downloading this build's pin;
	// status queries still never install. Verification at spawn is repeated.
	status, err := computeruse.New(home).Ensure(ctx)
	if err != nil {
		return ServerEntry{}, fmt.Errorf("managed Computer Use: %w", err)
	}
	entry.Command = status.Path
	entry.managedComputerUse = true
	// 'disabled' is an explicit non-tier value: the helper must not inherit a
	// configured implicit Terminal Full override. Per-app approval remains live.
	entry.Env = map[string]string{"METIS_CU_MANAGED": "1", "METIS_CU_HOST_TERMINAL_TIER": "disabled"}
	return entry, nil
}
