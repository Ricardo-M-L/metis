package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"time"

	"github.com/Ricardo-M-L/metis/internal/computeruse"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

const computerUseHelp = `metis cu — managed Computer Use component

  metis cu status [--json]          Inspect installation; never captures the screen
  metis cu install                 Install the release pinned by this METIS build
  metis cu install --from PATH     Explicitly install a locally built helper
  metis cu enable                  Install if needed and enable for the next session
  metis cu disable                 Disable startup in future sessions
  metis cu permissions-accessibility
  metis cu permissions-screen-recording

In an active CLI use /cu enable, /cu stop, /cu disable for immediate control.
Desktop offers the same controls in Settings → Computer Use.
OS permissions and app access are separate approvals; install does not grant them.
`

func cmdComputerUse(ctx context.Context, args []string) error {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Print(computerUseHelp)
		return nil
	}
	action := "status"
	if len(args) > 0 {
		action, args = args[0], args[1:]
	}
	jsonOutput := false
	localPath := ""
	for len(args) > 0 {
		switch args[0] {
		case "--json":
			jsonOutput = true
			args = args[1:]
		case "--from":
			if action != "install" || len(args) < 2 || localPath != "" {
				return fmt.Errorf("cu: --from requires one path and the install command")
			}
			localPath, args = args[1], args[2:]
		default:
			return fmt.Errorf("cu: unknown option %q", args[0])
		}
	}
	var status computeruse.Status
	var err error
	if localPath != "" {
		home, homeErr := config.VerifiedHome()
		if homeErr != nil {
			return homeErr
		}
		status, err = computeruse.New(home).InstallLocal(ctx, localPath)
	} else {
		status, err = (*runtime)(nil).computerUseAction(ctx, action)
	}
	if jsonOutput {
		if err != nil {
			status.Message = err.Error()
		}
		if encodeErr := json.NewEncoder(os.Stdout).Encode(status); encodeErr != nil {
			return encodeErr
		}
	} else {
		fmt.Printf("Computer Use: installed=%t enabled=%t running=%t\n", status.Installed, status.Enabled, status.Running)
		if status.Version != "" {
			fmt.Printf("Version: %s · source: %s\n", status.Version, status.Source)
		}
		if status.Path != "" {
			fmt.Printf("Path: %s\n", status.Path)
		}
		if status.Description != nil {
			fmt.Printf("OS permissions: accessibility=%s, screenRecording=%s\n", status.Description.Permissions["accessibility"], status.Description.Permissions["screenRecording"])
		}
		if status.Message != "" {
			fmt.Println(status.Message)
		}
	}
	return err
}

func (r *runtime) computerUseAction(ctx context.Context, action string) (computeruse.Status, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	switch action {
	case "status", "install", "enable", "stop", "disable", "permissions-accessibility", "permissions-screen-recording":
	default:
		return computeruse.Status{}, fmt.Errorf("unknown Computer Use action %q", action)
	}
	home, err := config.VerifiedHome()
	if err != nil {
		return computeruse.Status{}, err
	}
	manager := computeruse.New(home)
	switch action {
	case "status":
		return r.computerUseStatus(ctx, manager)
	case "install":
		return manager.Ensure(ctx)
	case "permissions-accessibility", "permissions-screen-recording":
		if err := openComputerUsePermissionSettings(ctx, action); err != nil {
			return computeruse.Status{}, err
		}
		status, err := r.computerUseStatus(ctx, manager)
		status.Message = "System Settings opened; permissions must be granted by you. Restart Computer Use and refresh status afterwards."
		return status, err
	case "stop", "disable":
		if r == nil && action == "stop" {
			return computeruse.Status{}, fmt.Errorf("use /cu stop in the active CLI or Stop in Desktop; this command does not own another session's process")
		}
		if r != nil {
			r.computerUseMu.Lock()
			defer r.computerUseMu.Unlock()
		}
		stopErr := r.stopComputerUseLocked()
		if action == "disable" {
			reg, loadErr := mcp.Load()
			if loadErr != nil {
				return computeruse.Status{}, errors.Join(stopErr, loadErr)
			}
			if entry := mcp.FindServer(reg, mcp.ReservedComputerUseName); entry != nil {
				entry.Disabled = true
				if err := mcp.Save(reg); err != nil {
					return computeruse.Status{}, errors.Join(stopErr, err)
				}
			}
		}
		status, statusErr := r.computerUseStatus(ctx, manager)
		status.Message = "Computer Use stopped in this session."
		if r == nil {
			status.Message = "Startup disabled. Other running sessions must be stopped using their own /cu stop or Desktop Stop control."
		}
		return status, errors.Join(stopErr, statusErr)
	case "enable":
		return r.enableComputerUse(ctx, manager)
	}
	panic("unreachable Computer Use action")
}

func (r *runtime) computerUseStatus(ctx context.Context, manager *computeruse.Manager) (computeruse.Status, error) {
	status, err := manager.Status(ctx)
	if err != nil {
		return status, err
	}
	reg, err := mcp.Load()
	if err != nil {
		return status, err
	}
	if entry := mcp.FindServer(reg, mcp.ReservedComputerUseName); entry != nil {
		status.Enabled = !entry.Disabled
		if entry.Command != mcp.ManagedComputerUseCommand {
			status.Message = "A custom/legacy Computer Use server is configured; it has not been replaced by the managed component."
		}
	}
	for _, srv := range r.computerUseServers() {
		if !srv.IsSpawned() {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		description, running, probeErr := readComputerUseStatus(probeCtx, srv)
		cancel()
		if probeErr != nil {
			status.Message = "Computer Use connection is not healthy: " + probeErr.Error()
			continue
		}
		status.Running = status.Running || running
		status.Description = description
	}
	return status, nil
}

// Resource URIs are private to the MCP client. Resolve the status resource
// through its current catalog instead of passing a raw URI to ReadResource.
func readComputerUseStatus(ctx context.Context, srv *mcptools.Server) (*computeruse.Description, bool, error) {
	resources, err := srv.ListResources(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, resource := range resources {
		if resource.Name != "Computer use status" {
			continue
		}
		result, err := srv.ReadResource(ctx, resource.URI)
		if err != nil {
			return nil, false, err
		}
		if result == nil {
			continue
		}
		for _, content := range result.Contents {
			var state struct {
				computeruse.Description
				Lifecycle struct {
					State string `json:"state"`
				} `json:"lifecycle"`
			}
			if json.Unmarshal([]byte(content.Text), &state) != nil || state.Name != "metis-cu" || state.ProtocolVersion != computeruse.ProtocolVersion {
				continue
			}
			return &state.Description, state.Lifecycle.State == "idle" || state.Lifecycle.State == "running", nil
		}
	}
	return nil, false, errors.New("compatible Computer Use status resource unavailable")
}

func (r *runtime) computerUseServers() []*mcptools.Server {
	if r == nil {
		return nil
	}
	r.mcpServersMu.Lock()
	defer r.mcpServersMu.Unlock()
	var out []*mcptools.Server
	for _, srv := range r.mcpServers {
		if srv != nil && srv.Name() == mcp.ReservedComputerUseName {
			out = append(out, srv)
		}
	}
	return out
}

func (r *runtime) enableComputerUse(ctx context.Context, manager *computeruse.Manager) (computeruse.Status, error) {
	// Validate the existing configuration before any installation or launch.
	reg, err := mcp.Load()
	if err != nil {
		return computeruse.Status{}, err
	}
	if err := mcp.SetManagedComputerUseServer(reg); err != nil {
		return computeruse.Status{}, err
	}
	var generation uint64
	var ticket *explicitMCPLaunchTicket
	if r != nil {
		r.computerUseMu.Lock()
		// Enabling an already connected helper is idempotent; a replacement
		// cannot acquire the machine-wide input lease while it is still live.
		if len(r.computerUseServers()) > 0 {
			current, statusErr := r.computerUseStatus(ctx, manager)
			if statusErr == nil && current.Running {
				r.computerUseMu.Unlock()
				return current, nil
			}
			if stopErr := r.stopComputerUseLocked(); stopErr != nil {
				r.computerUseMu.Unlock()
				return current, stopErr
			}
		}
		parent := r.sessionHeartbeatParent
		if parent == nil {
			parent = context.Background()
		}
		ticket = r.beginExplicitMCPLaunch(parent)
		defer ticket.Finish()
		r.mcpServersMu.Lock()
		r.computerUseGeneration++
		generation = r.computerUseGeneration
		priorCancel := r.computerUseCancel
		r.computerUseCancel = ticket.Cancel
		r.computerUseStopped = false
		r.mcpServersMu.Unlock()
		r.computerUseMu.Unlock()
		if priorCancel != nil {
			priorCancel()
		}
		stopCancel := context.AfterFunc(ctx, ticket.Cancel)
		defer stopCancel()
		ctx = ticket.Context()
	}
	status, err := manager.Ensure(ctx)
	if err != nil {
		if ticket != nil {
			ticket.Cancel()
		}
		return status, err
	}
	var srv *mcptools.Server
	staged := tools.NewRegistry()
	if r != nil {
		srv, err = mcp.LaunchServerWithSandbox(ctx, reg, mcp.ReservedComputerUseName, staged, r.sandbox)
		if err != nil {
			ticket.Cancel()
			return status, err
		}
		defer func() {
			if srv != nil {
				_ = srv.Close()
			}
		}()
		r.computerUseMu.Lock()
		defer r.computerUseMu.Unlock()
		r.mcpServersMu.Lock()
		stale := r.mcpClosing || r.computerUseStopped || generation != r.computerUseGeneration || ticket.epoch != r.mcpLaunchEpoch
		r.mcpServersMu.Unlock()
		if stale || ctx.Err() != nil {
			ticket.Cancel()
			return status, context.Canceled
		}
	}
	// Reload at commit so unrelated MCP edits during installation survive.
	reg, err = mcp.Load()
	if err != nil {
		return status, err
	}
	if err := mcp.SetManagedComputerUseServer(reg); err != nil {
		return status, err
	}
	if err := mcp.Save(reg); err != nil {
		return status, err
	}
	if r != nil {
		r.adoptMCPServerAtGeneration(srv, staged.All(), true, ticket.epoch, &generation)
		srv = nil // ownership consumed even when a concurrent stop rejected it
	}
	status, err = r.computerUseStatus(ctx, manager)
	if r == nil {
		status.Message = "Enabled for the next session. In an open CLI use /cu enable; in Desktop click Enable for immediate connection."
	}
	return status, err
}

func (r *runtime) stopComputerUse() error {
	if r == nil {
		return nil
	}
	r.computerUseMu.Lock()
	defer r.computerUseMu.Unlock()
	return r.stopComputerUseLocked()
}

func (r *runtime) stopComputerUseLocked() error {
	if r == nil {
		return nil
	}
	r.mcpServersMu.Lock()
	r.computerUseGeneration++
	r.computerUseStopped = true
	cancel := r.computerUseCancel
	r.computerUseCancel = nil
	if r.mcpExplicitServers == nil {
		r.mcpExplicitServers = make(map[string]struct{})
	}
	r.mcpExplicitServers[mcp.ReservedComputerUseName] = struct{}{}
	var stopped []*mcptools.Server
	kept := r.mcpServers[:0]
	for _, srv := range r.mcpServers {
		if srv != nil && srv.Name() == mcp.ReservedComputerUseName {
			stopped = append(stopped, srv)
		} else {
			kept = append(kept, srv)
		}
	}
	r.mcpServers = kept
	if r.registry != nil {
		r.registry.ReplacePrefix("mcp__computer-use__", nil)
	}
	if r.slashRegistry != nil {
		r.slashRegistry.RemoveSource("mcp:" + mcp.ReservedComputerUseName)
	}
	r.mcpServersMu.Unlock()
	var errs []error
	// Managed Server.Close owns the native stop ACK before transport kill.
	for _, srv := range stopped {
		if err := srv.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if cancel != nil {
		cancel()
	}
	return errors.Join(errs...)
}

func (r *runtime) endComputerUseTurn() {
	if r == nil {
		return
	}
	r.mcpServersMu.Lock()
	generation := r.computerUseGeneration
	servers := append([]*mcptools.Server(nil), r.mcpServers...)
	r.mcpServersMu.Unlock()
	for _, srv := range servers {
		if srv == nil || srv.Name() != mcp.ReservedComputerUseName {
			continue
		}
		if !srv.IsSpawned() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := srv.ComputerUseControl(ctx, "end-turn")
		cancel()
		if err != nil {
			// A helper that cannot confirm release is not safe for another turn.
			r.computerUseMu.Lock()
			r.mcpServersMu.Lock()
			current := r.computerUseGeneration == generation
			found := false
			for _, candidate := range r.mcpServers {
				if candidate == srv {
					found = true
				}
			}
			r.mcpServersMu.Unlock()
			if current && found {
				_ = r.stopComputerUseLocked()
				fmt.Fprintf(os.Stderr, "metis: Computer Use cleanup failed; component stopped: %v\n", err)
			}
			r.computerUseMu.Unlock()
			return
		}
	}
}

func openComputerUsePermissionSettings(ctx context.Context, action string) error {
	if goruntime.GOOS != "darwin" {
		return fmt.Errorf("automatic OS permission settings navigation is only supported on macOS")
	}
	var pane string
	switch action {
	case "permissions-accessibility":
		pane = "Privacy_Accessibility"
	case "permissions-screen-recording":
		pane = "Privacy_ScreenCapture"
	default:
		return fmt.Errorf("unknown permission settings action")
	}
	return exec.CommandContext(ctx, "/usr/bin/open", "x-apple.systempreferences:com.apple.preference.security?"+pane).Run()
}
