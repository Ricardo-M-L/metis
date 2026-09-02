package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

func mcpToolsForServer(registry *tools.Registry, serverName string) []tools.Tool {
	if registry == nil {
		return nil
	}
	prefix := "mcp__" + serverName + "__"
	out := make([]tools.Tool, 0)
	for _, candidate := range registry.All() {
		if len(candidate.Name()) >= len(prefix) && candidate.Name()[:len(prefix)] == prefix {
			out = append(out, candidate)
		}
	}
	return out
}

// adoptMCPServer makes the live server list and its tool namespace one
// ownership transaction. Explicit reauthentication has priority over the
// asynchronous startup launcher; a late startup result is closed and cannot
// republish stale tools or a sticky-failed client.
func (r *runtime) adoptMCPServer(server *mcptools.Server, discovered []tools.Tool, explicit bool) bool {
	return r.adoptMCPServerAtEpoch(server, discovered, explicit, r.currentMCPLaunchEpoch())
}

func (r *runtime) currentMCPLaunchEpoch() uint64 {
	if r == nil {
		return 0
	}
	r.mcpServersMu.Lock()
	defer r.mcpServersMu.Unlock()
	return r.mcpLaunchEpoch
}

// explicitMCPLaunchTicket is both an adoption generation and an in-flight
// lifecycle lease. revokeFullAccessResources cancels every ticket captured in
// the generation it cuts and waits for Finish before returning.
type explicitMCPLaunchTicket struct {
	runtime *runtime
	epoch   uint64
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
}

func (t *explicitMCPLaunchTicket) Context() context.Context {
	if t == nil || t.ctx == nil {
		return context.Background()
	}
	return t.ctx
}

func (t *explicitMCPLaunchTicket) Adopt(server *mcptools.Server, discovered []tools.Tool) bool {
	if t == nil || t.runtime == nil {
		if server != nil {
			_ = server.Close()
		}
		return true
	}
	return t.runtime.adoptMCPServerAtEpoch(server, discovered, true, t.epoch)
}

func (t *explicitMCPLaunchTicket) Cancel() {
	if t != nil && t.cancel != nil {
		t.cancel()
	}
}

// Finish releases the in-flight lease exactly once. Callers must invoke it
// only after every launcher goroutine has returned and every unadopted server
// has been closed; that makes done a true process-lifecycle barrier.
func (t *explicitMCPLaunchTicket) Finish() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		if t.runtime != nil {
			t.runtime.mcpServersMu.Lock()
			delete(t.runtime.mcpExplicitLaunches, t)
			t.runtime.mcpServersMu.Unlock()
		}
		close(t.done)
	})
}

func (t *explicitMCPLaunchTicket) Wait() {
	if t != nil && t.done != nil {
		<-t.done
	}
}

// beginExplicitMCPLaunch snapshots the adoption generation and registers its
// cancellable lease in one lifecycle critical section. UI callers therefore
// cannot resample the epoch after a slow handshake, and a concurrent revoke
// cannot miss a launch that has already begun.
func (r *runtime) beginExplicitMCPLaunch(parent context.Context) *explicitMCPLaunchTicket {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	ticket := &explicitMCPLaunchTicket{
		runtime: r,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	if r == nil {
		cancel()
		ticket.Finish()
		return ticket
	}

	r.mcpServersMu.Lock()
	ticket.epoch = r.mcpLaunchEpoch
	closing := r.mcpClosing
	if !closing {
		if r.mcpExplicitLaunches == nil {
			r.mcpExplicitLaunches = make(map[*explicitMCPLaunchTicket]struct{})
		}
		r.mcpExplicitLaunches[ticket] = struct{}{}
	}
	r.mcpServersMu.Unlock()
	if closing {
		cancel()
		ticket.Finish()
	}
	return ticket
}

// adoptMCPServerAtEpoch is the asynchronous-launch ownership boundary. An
// epoch mismatch means a safer permission mode already cancelled that launch;
// consume and close the late result so callers never fallback-publish it.
func (r *runtime) adoptMCPServerAtEpoch(server *mcptools.Server, discovered []tools.Tool, explicit bool, epoch uint64) bool {
	if r == nil || server == nil {
		return false
	}
	name := server.Name()
	r.mcpServersMu.Lock()
	if r.mcpClosing || epoch != r.mcpLaunchEpoch {
		r.mcpServersMu.Unlock()
		_ = server.Close()
		// true means the ownership callback consumed the server. Returning
		// false would tell the TUI to fallback-publish this already-closed late
		// result into the live registry.
		return true
	}
	if r.mcpExplicitServers == nil {
		r.mcpExplicitServers = make(map[string]struct{})
	}
	if !explicit {
		if _, replaced := r.mcpExplicitServers[name]; replaced {
			r.mcpServersMu.Unlock()
			_ = server.Close()
			return false
		}
	} else {
		r.mcpExplicitServers[name] = struct{}{}
	}

	prior := make([]*mcptools.Server, 0, 1)
	kept := r.mcpServers[:0]
	for _, current := range r.mcpServers {
		if current != nil && current.Name() == name {
			if current != server {
				prior = append(prior, current)
			}
			continue
		}
		kept = append(kept, current)
	}
	r.mcpServers = append(kept, server)
	if r.registry != nil {
		r.registry.ReplacePrefix("mcp__"+name+"__", discovered)
	}
	r.mcpServersMu.Unlock()

	for _, old := range prior {
		_ = old.Close()
	}
	return true
}

// onPermissionModeChanged records the ordered Gate transition and revokes
// process-owned work exactly when fullAccess is left. The new Gate mode has
// already committed, so no new model action can acquire the old approval
// posture while revocation runs.
func (r *runtime) onPermissionModeChanged(next permission.Mode) {
	if r.recordPermissionModeChange(next) {
		r.revokeFullAccessResources()
	}
}

// recordPermissionModeChange updates the listener-owned transition state and
// reports whether the edge leaves fullAccess. Splitting the record from the
// revocation lets the production listener restore the new sandbox posture
// before it waits on process cleanup, while focused lifecycle tests and reduced
// embedders can still use onPermissionModeChanged as an atomic convenience.
func (r *runtime) recordPermissionModeChange(next permission.Mode) bool {
	if r == nil {
		return false
	}
	next = permission.CanonicalMode(string(next))
	r.permissionModeMu.Lock()
	previous := r.permissionMode
	r.permissionMode = next
	leavingFullAccess := previous == permission.ModeFullAccess && next != permission.ModeFullAccess
	r.permissionModeMu.Unlock()
	return leavingFullAccess
}

// revokeFullAccessResources stops every long-lived operation that may have
// captured the unsandboxed process posture. Sandboxing cannot be retrofitted
// onto an existing process, so MCP/Computer Use must reconnect explicitly
// after the transition. Jobs, monitors and sub-agents remain reusable through
// their stable registries, but the old generation is cancelled and forgotten.
func (r *runtime) revokeFullAccessResources() {
	if r == nil {
		return
	}

	r.mcpServersMu.Lock()
	r.mcpLaunchEpoch++
	explicitLaunches := make([]*explicitMCPLaunchTicket, 0, len(r.mcpExplicitLaunches))
	for launch := range r.mcpExplicitLaunches {
		explicitLaunches = append(explicitLaunches, launch)
	}
	cancelLauncher := r.mcpLauncherCancel
	r.mcpLauncherCancel = nil
	launcherDone := r.mcpLauncherDone
	servers := append([]*mcptools.Server(nil), r.mcpServers...)
	r.mcpServers = nil
	r.mcpExplicitServers = nil
	if r.registry != nil {
		// Includes the reserved computer-use namespace and plugin-contributed
		// MCP tools. Non-MCP built-ins remain installed.
		r.registry.ReplacePrefix("mcp__", nil)
	}
	if r.slashRegistry != nil {
		// MCP prompts are slash commands backed by the same live client. Drop
		// every MCP source now; an explicit reconnect re-registers fresh ones.
		for _, command := range r.slashRegistry.Catalog() {
			if strings.HasPrefix(command.Source, "mcp:") {
				r.slashRegistry.RemoveSource(command.Source)
			}
		}
	}
	r.mcpServersMu.Unlock()

	if cancelLauncher != nil {
		cancelLauncher()
	}
	for _, launch := range explicitLaunches {
		launch.Cancel()
	}
	// Join explicit handshakes before any security-boundary cleanup can return.
	// Their Finish edge is emitted by the launcher path, not by adoption, so a
	// cancellation-insensitive launcher cannot be mistaken for a stopped one.
	for _, launch := range explicitLaunches {
		launch.Wait()
	}
	// Stop and join every producer before closing dependencies it may still be
	// calling. Session changes and fullAccess revocation now share this strict
	// lifecycle rule: neither boundary may return while an old runner/process is
	// still alive.
	r.releaseFullAccessWorkAndWait()
	if launcherDone != nil {
		<-launcherDone
	}
	for _, server := range servers {
		_ = server.Close()
	}
	if r.plugins != nil {
		if err := r.plugins.CloseMCPServers(); err != nil {
			fmt.Fprintf(os.Stderr, "metis: revoke fullAccess plugin MCP: %v\n", err)
		}
	}
}

func (r *runtime) releaseFullAccessWorkAndWait() {
	if r == nil {
		return
	}
	if r.cronSvc != nil {
		r.cronSvc.ClearEphemeral()
	}
	if r.loop != nil && r.loop.Monitors != nil {
		r.loop.Monitors.StopAll()
	}

	// Stop agent producers first. A canceled runner may race one final job
	// registration while unwinding; cutting the jobs generation only after all
	// runners are gone ensures that last process is included in the join.
	if r.subAgentRoster != nil {
		if err := r.subAgentRoster.CancelAndWait(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "metis: revoke fullAccess sub-agent: %v\n", err)
		}
	}
	if r.loop != nil && r.loop.Jobs != nil {
		r.loop.Jobs.ResetAndWait(0)
	}
}

func (r *runtime) registerLiveMCPPrompts(ctx context.Context, registry *slash.Registry, server *mcptools.Server) {
	if r == nil || registry == nil || server == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	handles := mcp.CollectPrompts(probeCtx, []*mcptools.Server{server})
	cancel()

	// Discovery is intentionally outside the lifecycle mutex. Re-check the
	// exact pointer before publishing so a slower prior reconnect cannot remove
	// or overwrite prompts from a newer explicit login.
	r.mcpServersMu.Lock()
	current := false
	if !r.mcpClosing {
		for _, candidate := range r.mcpServers {
			if candidate == server {
				current = true
				break
			}
		}
	}
	if !current {
		r.mcpServersMu.Unlock()
		return
	}
	registry.RemoveSource("mcp:" + server.Name())
	_ = registerMCPPromptsAsSlash(registry, handles)
	r.mcpServersMu.Unlock()
}

// startMCPPromptTask establishes a shutdown-safe Add/Wait boundary. Add is
// serialized with mcpClosing, so Cleanup can mark closing and then Wait without
// racing a new prompt goroutine into the WaitGroup.
func (r *runtime) startMCPPromptTask(run func(context.Context)) {
	if r == nil || run == nil {
		return
	}
	r.mcpServersMu.Lock()
	if r.mcpClosing {
		r.mcpServersMu.Unlock()
		return
	}
	ctx := r.mcpPromptCtx
	if ctx == nil {
		ctx = context.Background()
	}
	r.mcpPromptWG.Add(1)
	r.mcpServersMu.Unlock()
	go func() {
		defer r.mcpPromptWG.Done()
		run(ctx)
	}()
}

func (r *runtime) scheduleLiveMCPPrompts(registry *slash.Registry, server *mcptools.Server) {
	if registry == nil || server == nil {
		return
	}
	r.startMCPPromptTask(func(ctx context.Context) {
		r.registerLiveMCPPrompts(ctx, registry, server)
	})
}

// scheduleMCPPromptsAfterLaunch closes the startup race where buildSlash takes
// its initial server snapshot before the asynchronous launcher finishes. The
// launcher-done channel is immutable after runtime construction, so waiting on
// it is safe alongside WaitForMCP and Cleanup.
func (r *runtime) scheduleMCPPromptsAfterLaunch(registry *slash.Registry) {
	if r == nil || registry == nil {
		return
	}
	done := r.mcpLauncherDone
	r.startMCPPromptTask(func(ctx context.Context) {
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return
			}
		}
		r.mcpServersMu.Lock()
		if r.mcpClosing {
			r.mcpServersMu.Unlock()
			return
		}
		snapshot := append([]*mcptools.Server(nil), r.mcpServers...)
		r.mcpServersMu.Unlock()
		for _, server := range snapshot {
			r.registerLiveMCPPrompts(ctx, registry, server)
		}
	})
}
