package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	mcpsdk "github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

func lazyLifecycleServer(name string, toolNames ...string) *mcptools.Server {
	cached := make([]mcpsdk.Tool, 0, len(toolNames))
	for _, toolName := range toolNames {
		cached = append(cached, mcpsdk.Tool{Name: toolName})
	}
	return mcptools.NewLazyServer(name, cached, func(context.Context) (*mcpsdk.Client, error) {
		return nil, context.Canceled
	})
}

func TestRuntimeLifecycleBlockingJobHelper(t *testing.T) {
	if os.Getenv("METIS_TEST_BLOCKING_LIFECYCLE_JOB") != "1" {
		return
	}
	select {}
}

func TestRuntimeExplicitMCPAdoptionReplacesNamespaceAndRejectsLateStartup(t *testing.T) {
	rt := &runtime{registry: tools.NewRegistry()}
	old := lazyLifecycleServer("secure", "keep", "removed")
	if !rt.adoptMCPServer(old, old.Tools(), false) {
		t.Fatal("initial server was not adopted")
	}
	fresh := lazyLifecycleServer("secure", "keep", "added")
	if !rt.adoptMCPServer(fresh, fresh.Tools(), true) {
		t.Fatal("explicit server was not adopted")
	}
	if len(rt.mcpServers) != 1 || rt.mcpServers[0] != fresh {
		t.Fatalf("live servers after explicit adoption = %#v", rt.mcpServers)
	}
	if _, ok := rt.registry.Get("mcp__secure__removed"); ok {
		t.Fatal("stale tool from replaced server survived")
	}
	if _, ok := rt.registry.Get("mcp__secure__added"); !ok {
		t.Fatal("fresh server tool was not published")
	}

	late := lazyLifecycleServer("secure", "late")
	if rt.adoptMCPServer(late, late.Tools(), false) {
		t.Fatal("late startup server replaced explicit connection")
	}
	if _, ok := rt.registry.Get("mcp__secure__late"); ok {
		t.Fatal("late startup tools overwrote explicit namespace")
	}
}

func TestRuntimeMCPAdoptionHonorsInstalledVisibilityPolicy(t *testing.T) {
	reg := tools.NewRegistry()
	tools.ApplyToolVisibility(reg, nil, []string{"mcp__blocked"})
	rt := &runtime{registry: reg}
	server := lazyLifecycleServer("blocked", "late")
	if !rt.adoptMCPServer(server, server.Tools(), false) {
		t.Fatal("server ownership should be adopted even when all tools are filtered")
	}
	if _, ok := reg.Get("mcp__blocked__late"); ok {
		t.Fatal("late MCP adoption bypassed the installed visibility policy")
	}
	if len(rt.mcpServers) != 1 || rt.mcpServers[0] != server {
		t.Fatalf("resource/prompt server ownership was lost: %#v", rt.mcpServers)
	}
}

func TestRuntimeAdoptsResourceOnlyMCPServer(t *testing.T) {
	rt := &runtime{registry: tools.NewRegistry()}
	resourceOnly := lazyLifecycleServer("resources")
	if !rt.adoptMCPServer(resourceOnly, nil, true) {
		t.Fatal("resource-only server was not adopted")
	}
	if len(rt.mcpServers) != 1 || rt.mcpServers[0] != resourceOnly {
		t.Fatalf("resource-only live servers = %#v", rt.mcpServers)
	}
}

func TestRuntimeRejectsMCPAdoptionAfterCleanupStarts(t *testing.T) {
	rt := &runtime{registry: tools.NewRegistry(), mcpClosing: true}
	late := lazyLifecycleServer("late", "unsafe")
	if !rt.adoptMCPServer(late, late.Tools(), false) {
		t.Fatal("closing runtime did not consume the late MCP server")
	}
	if len(rt.mcpServers) != 0 {
		t.Fatalf("live servers after closing adoption = %#v", rt.mcpServers)
	}
	if _, ok := rt.registry.Get("mcp__late__unsafe"); ok {
		t.Fatal("late MCP tool was published after cleanup started")
	}
}

func TestRuntimeLeavingFullAccessRevokesResourcesAndRejectsLateLaunch(t *testing.T) {
	registry := tools.NewRegistry()
	slashRegistry := slash.NewRegistry()
	slashRegistry.Register(slash.Cmd{Name: "mcp__unsafe__prompt", Source: "mcp:unsafe"})
	slashRegistry.Register(slash.Cmd{Name: "help", Source: "slash"})
	roster := agent.NewRoster(2)
	jobRegistry := jobs.NewRegistry(t.TempDir())
	jobCtx, cancelJob := context.WithCancel(context.Background())
	t.Cleanup(cancelJob)
	command := exec.CommandContext(jobCtx, os.Args[0], "-test.run=^TestRuntimeLifecycleBlockingJobHelper$")
	command.Env = append(os.Environ(), "METIS_TEST_BLOCKING_LIFECYCLE_JOB=1")
	if _, err := jobRegistry.Spawn(jobs.SpawnArgs{Command: "test-blocking-job", Cmd: command, Cancel: cancelJob}); err != nil {
		t.Fatalf("spawn lifecycle test job: %v", err)
	}
	cancelled := make(chan struct{})
	teammate := &agent.Teammate{
		Name:   "full-access-worker",
		Status: agent.StatusRunning,
		Cancel: func() { close(cancelled) },
	}
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-cancelled
		roster.UnregisterTeammate(teammate)
	}()
	rt := &runtime{
		registry:       registry,
		slashRegistry:  slashRegistry,
		subAgentRoster: roster,
		loop:           &agent.Loop{Jobs: jobRegistry, Monitors: agent.NewMonitorRegistry(1)},
		permissionMode: permission.ModeFullAccess,
		mcpLaunchEpoch: 7,
	}
	launchEpoch := rt.currentMCPLaunchEpoch()
	server := lazyLifecycleServer("unsafe", "run")
	if !rt.adoptMCPServerAtEpoch(server, server.Tools(), false, launchEpoch) {
		t.Fatal("current fullAccess launch was not adopted")
	}

	rt.onPermissionModeChanged(permission.ModeDefault)

	select {
	case <-cancelled:
	default:
		t.Fatal("fullAccess sub-agent was not cancelled")
	}
	if roster.Count() != 0 {
		t.Fatalf("roster count after revocation = %d", roster.Count())
	}
	if got := jobRegistry.List(); len(got) != 0 {
		t.Fatalf("background jobs survived fullAccess revocation: %#v", got)
	}
	if command.ProcessState == nil {
		t.Fatal("fullAccess revocation returned before the job cmd.Wait path reaped its process")
	}
	if len(rt.mcpServers) != 0 {
		t.Fatalf("MCP servers survived fullAccess revocation: %#v", rt.mcpServers)
	}
	if _, ok := registry.Get("mcp__unsafe__run"); ok {
		t.Fatal("MCP tool namespace survived fullAccess revocation")
	}
	if _, ok := slashRegistry.Resolve("mcp__unsafe__prompt"); ok {
		t.Fatal("MCP prompt closure survived fullAccess revocation")
	}
	if _, ok := slashRegistry.Resolve("help"); !ok {
		t.Fatal("MCP prompt cleanup removed an unrelated slash command")
	}

	late := lazyLifecycleServer("late", "escape")
	if !rt.adoptMCPServerAtEpoch(late, late.Tools(), false, launchEpoch) {
		t.Fatal("stale launcher result was not consumed")
	}
	if _, ok := registry.Get("mcp__late__escape"); ok {
		t.Fatal("cancelled fullAccess launcher republished a late tool")
	}
	if len(rt.mcpServers) != 0 {
		t.Fatalf("late server was re-adopted: %#v", rt.mcpServers)
	}

	// Revocation invalidates only the stale startup generation. A deliberate
	// reconnect under the newly active safe posture must remain possible.
	fresh := lazyLifecycleServer("safe-reconnect", "read")
	if !rt.adoptMCPServer(fresh, fresh.Tools(), true) {
		t.Fatal("explicit safe-mode reconnect was rejected")
	}
	if _, ok := registry.Get("mcp__safe-reconnect__read"); !ok {
		t.Fatal("explicit safe-mode reconnect did not publish tools")
	}
}

func TestRuntimeExplicitLaunchTicketRejectsAdoptionAfterFullAccessRevocation(t *testing.T) {
	registry := tools.NewRegistry()
	slashRegistry := slash.NewRegistry()
	rt := &runtime{
		registry:       registry,
		slashRegistry:  slashRegistry,
		permissionMode: permission.ModeFullAccess,
		mcpLaunchEpoch: 41,
		mcpPromptCtx:   context.Background(),
	}
	ticket := rt.beginExplicitMCPLaunch(context.Background())

	// The explicit reconnect started while fullAccess was active, but its
	// handshake did not finish until the safer posture had revoked that launch
	// generation. The ticket must retain epoch 41 rather than sampling the new
	// epoch at adoption time.
	revoked := make(chan struct{})
	go func() {
		rt.onPermissionModeChanged(permission.ModeDefault)
		close(revoked)
	}()
	select {
	case <-ticket.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel the stale explicit ticket")
	}
	late := lazyLifecycleServer("explicit-late", "unsafe")
	if !ticket.Adopt(late, late.Tools()) {
		t.Fatal("stale explicit launch result was not consumed")
	}
	ticket.Finish()
	select {
	case <-revoked:
	case <-time.After(time.Second):
		t.Fatal("revocation did not finish after the late result was closed")
	}
	if len(rt.mcpExplicitLaunches) != 0 {
		t.Fatalf("revocation retained stale explicit launch leases: %d", len(rt.mcpExplicitLaunches))
	}
	if len(rt.mcpServers) != 0 {
		t.Fatalf("stale explicit server was adopted: %#v", rt.mcpServers)
	}
	if _, ok := registry.Get("mcp__explicit-late__unsafe"); ok {
		t.Fatal("stale explicit launch published tools after revocation")
	}
	// cmdChat schedules prompt discovery after every consumed adoption. The
	// pointer-identity recheck must also make that harmless for a consumed stale
	// result.
	rt.scheduleLiveMCPPrompts(slashRegistry, late)
	rt.mcpPromptWG.Wait()
	for _, command := range slashRegistry.Catalog() {
		if command.Source == "mcp:explicit-late" {
			t.Fatalf("stale explicit launch published prompt %q", command.Name)
		}
	}

	// A rejected adoption owns and closes the late result. Lazy execution is a
	// deterministic observation of that close without spawning a subprocess.
	result, err := late.Tools()[0].Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("probe closed late server: %v", err)
	}
	if result == nil || !result.IsError || !strings.Contains(strings.ToLower(result.Output), "closed") {
		t.Fatalf("late server was not closed by stale ticket: %#v", result)
	}

	// A reconnect deliberately begun after revocation captures the new epoch and
	// remains usable under the active safe sandbox posture.
	freshTicket := rt.beginExplicitMCPLaunch(context.Background())
	defer freshTicket.Finish()
	fresh := lazyLifecycleServer("explicit-fresh", "safe")
	defer fresh.Close()
	if !freshTicket.Adopt(fresh, fresh.Tools()) {
		t.Fatal("fresh explicit launch ticket was rejected")
	}
	if _, ok := registry.Get("mcp__explicit-fresh__safe"); !ok {
		t.Fatal("fresh explicit launch did not publish its tools")
	}
}

func TestRuntimeFullAccessRevocationCancelsAndJoinsExplicitLaunch(t *testing.T) {
	rt := &runtime{permissionMode: permission.ModeFullAccess, mcpLaunchEpoch: 9}
	ticket := rt.beginExplicitMCPLaunch(context.Background())
	launchCanceled := make(chan struct{})
	launchRelease := make(chan struct{})
	go func() {
		<-ticket.Context().Done()
		close(launchCanceled)
		<-launchRelease
		ticket.Finish()
	}()

	revoked := make(chan struct{})
	go func() {
		rt.onPermissionModeChanged(permission.ModeDefault)
		close(revoked)
	}()
	select {
	case <-launchCanceled:
	case <-time.After(time.Second):
		t.Fatal("fullAccess revocation did not cancel the explicit launch")
	}
	select {
	case <-revoked:
		close(launchRelease)
		t.Fatal("fullAccess revocation returned before the explicit launch released its lease")
	default:
	}
	close(launchRelease)
	select {
	case <-revoked:
	case <-time.After(time.Second):
		t.Fatal("fullAccess revocation did not return after the explicit launch finished")
	}
	if len(rt.mcpExplicitLaunches) != 0 {
		t.Fatalf("fullAccess revocation retained explicit launch leases: %d", len(rt.mcpExplicitLaunches))
	}
}

func TestRuntimeCleanupCancelsAndJoinsExplicitLaunch(t *testing.T) {
	rt := &runtime{mcpLaunchEpoch: 13}
	ticket := rt.beginExplicitMCPLaunch(context.Background())
	launchCanceled := make(chan struct{})
	launchRelease := make(chan struct{})
	go func() {
		<-ticket.Context().Done()
		close(launchCanceled)
		<-launchRelease // emulate cancellation-insensitive launcher cleanup
		ticket.Finish()
	}()

	cleaned := make(chan struct{})
	go func() {
		rt.Cleanup()
		close(cleaned)
	}()
	select {
	case <-launchCanceled:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not cancel the explicit launch")
	}
	select {
	case <-cleaned:
		close(launchRelease)
		t.Fatal("cleanup returned before the explicit launch released its lease")
	default:
	}
	close(launchRelease)
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not return after the explicit launch finished")
	}
	if len(rt.mcpExplicitLaunches) != 0 {
		t.Fatalf("cleanup retained explicit launch leases: %d", len(rt.mcpExplicitLaunches))
	}
	if !rt.mcpClosing {
		t.Fatal("cleanup did not close the explicit launch admission gate")
	}
}

func TestRuntimeBeginExplicitLaunchAfterCleanupIsRejectedAndFinished(t *testing.T) {
	rt := &runtime{mcpClosing: true, mcpLaunchEpoch: 17}
	ticket := rt.beginExplicitMCPLaunch(context.Background())
	select {
	case <-ticket.Context().Done():
	default:
		t.Fatal("launch begun after cleanup did not receive a canceled context")
	}
	done := make(chan struct{})
	go func() {
		ticket.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("rejected post-cleanup launch retained an in-flight lease")
	}
	if len(rt.mcpExplicitLaunches) != 0 {
		t.Fatalf("post-cleanup launch entered live lease map: %d", len(rt.mcpExplicitLaunches))
	}
}

func TestRuntimeLeavingFullAccessJoinsRosterRunnerAndMCPLauncher(t *testing.T) {
	roster := agent.NewRoster(1)
	rosterCancelled := make(chan struct{})
	rosterRelease := make(chan struct{})
	teammate := &agent.Teammate{
		Name: "full-access-runner",
		Cancel: func() {
			select {
			case <-rosterCancelled:
			default:
				close(rosterCancelled)
			}
		},
	}
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-rosterCancelled
		<-rosterRelease
		roster.UnregisterTeammate(teammate)
	}()

	launcherCancelled := make(chan struct{})
	launcherRelease := make(chan struct{})
	launcherDone := make(chan struct{})
	rt := &runtime{
		subAgentRoster: roster,
		permissionMode: permission.ModeFullAccess,
		mcpLauncherCancel: func() {
			select {
			case <-launcherCancelled:
			default:
				close(launcherCancelled)
			}
		},
		mcpLauncherDone: launcherDone,
	}
	go func() {
		<-launcherCancelled
		<-launcherRelease
		// A real launcher may need the lifecycle mutex to consume a late
		// handshake before its defer closes done. Revoke must never join it
		// while retaining this mutex.
		rt.mcpServersMu.Lock()
		rt.mcpServersMu.Unlock()
		close(launcherDone)
	}()
	revoked := make(chan struct{})
	go func() {
		rt.onPermissionModeChanged(permission.ModeDefault)
		close(revoked)
	}()
	select {
	case <-rosterCancelled:
	case <-time.After(time.Second):
		t.Fatal("fullAccess revoke did not cancel the roster runner")
	}
	select {
	case <-launcherCancelled:
	case <-time.After(time.Second):
		close(rosterRelease)
		t.Fatal("fullAccess revoke did not cancel the MCP launcher")
	}
	select {
	case <-revoked:
		close(rosterRelease)
		close(launcherRelease)
		t.Fatal("fullAccess revoke returned before roster runner and MCP launcher exited")
	default:
	}
	close(rosterRelease)
	select {
	case <-revoked:
		close(launcherRelease)
		t.Fatal("fullAccess revoke returned before MCP launcher exited")
	default:
	}
	close(launcherRelease)
	select {
	case <-revoked:
	case <-time.After(time.Second):
		t.Fatal("fullAccess revoke did not return after all producers exited")
	}
}

func TestRuntimeFullAccessSessionBoundaryJoinsBeforeListenerRevokesMCP(t *testing.T) {
	registry := tools.NewRegistry()
	roster := agent.NewRoster(1)
	jobRegistry := jobs.NewRegistry(t.TempDir())
	jobCtx, cancelJob := context.WithCancel(context.Background())
	defer cancelJob()
	command := exec.CommandContext(jobCtx, os.Args[0], "-test.run=^TestRuntimeLifecycleBlockingJobHelper$")
	command.Env = append(os.Environ(), "METIS_TEST_BLOCKING_LIFECYCLE_JOB=1")
	job, err := jobRegistry.Spawn(jobs.SpawnArgs{Command: "source-session-job", Cmd: command, Cancel: cancelJob})
	if err != nil {
		t.Fatal(err)
	}

	runnerCancelled := make(chan struct{})
	runnerRelease := make(chan struct{})
	runnerReleased := false
	defer func() {
		if !runnerReleased {
			close(runnerRelease)
		}
	}()
	teammate := &agent.Teammate{
		Name: "source-session-runner",
		Cancel: func() {
			select {
			case <-runnerCancelled:
			default:
				close(runnerCancelled)
			}
		},
	}
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-runnerCancelled
		<-runnerRelease
		roster.UnregisterTeammate(teammate)
	}()

	rt := &runtime{
		registry:       registry,
		subAgentRoster: roster,
		loop:           &agent.Loop{Jobs: jobRegistry, Monitors: agent.NewMonitorRegistry(1)},
		permissionMode: permission.ModeFullAccess,
	}
	server := lazyLifecycleServer("source-mcp", "unsafe")
	if !rt.adoptMCPServer(server, server.Tools(), false) {
		t.Fatal("source MCP server was not adopted")
	}

	boundaryDone := make(chan struct{})
	go func() {
		rt.releaseSessionWork()
		close(boundaryDone)
	}()
	select {
	case <-runnerCancelled:
	case <-time.After(time.Second):
		t.Fatal("session boundary did not cancel the fullAccess runner")
	}
	// CancelAndWait retains the addressable source generation until the runner
	// unregisters. The old non-blocking Reset clears this map before unlocking,
	// so Count is a deterministic discriminator without a timing sleep.
	if got := roster.Count(); got != 1 {
		t.Fatalf("fullAccess session boundary forgot its live runner before join: count=%d", got)
	}
	select {
	case <-boundaryDone:
		t.Fatal("fullAccess session boundary returned before its runner exited")
	default:
	}
	if snapshot, ok := jobRegistry.Get(job.ID); !ok || snapshot.Status != jobs.StatusRunning {
		t.Fatalf("fullAccess session boundary reset jobs before its producer runner exited: ok=%v job=%+v", ok, snapshot)
	}
	if _, ok := registry.Get("mcp__source-mcp__unsafe"); !ok {
		t.Fatal("session boundary revoked MCP before the permission listener edge")
	}

	close(runnerRelease)
	runnerReleased = true
	select {
	case <-boundaryDone:
	case <-time.After(3 * time.Second):
		t.Fatal("session boundary did not finish after its runner was released")
	}
	if command.ProcessState == nil {
		t.Fatal("fullAccess session boundary returned before the job cmd.Wait path reaped its process")
	}
	if roster.Count() != 0 {
		t.Fatal("fullAccess session boundary retained its joined roster generation")
	}
	if _, ok := registry.Get("mcp__source-mcp__unsafe"); !ok {
		t.Fatal("strict session work cleanup consumed listener-owned MCP resources")
	}

	rt.onPermissionModeChanged(permission.ModeDefault)
	if _, ok := registry.Get("mcp__source-mcp__unsafe"); ok {
		t.Fatal("fullAccess listener did not revoke MCP after the strict session boundary")
	}
	if len(rt.mcpServers) != 0 {
		t.Fatalf("MCP handles survived listener revoke: %+v", rt.mcpServers)
	}
}

func TestRegisterLiveMCPPromptsDropsStalePromptFromPriorServer(t *testing.T) {
	registry := slash.NewRegistry()
	registry.Register(slash.Cmd{Name: "mcp__resources__stale", Source: "mcp:resources"})
	registry.Register(slash.Cmd{Name: "help", Source: "slash"})
	server := lazyLifecycleServer("resources")
	rt := &runtime{mcpServers: []*mcptools.Server{server}}

	rt.registerLiveMCPPrompts(context.Background(), registry, server)

	if _, ok := registry.Resolve("mcp__resources__stale"); ok {
		t.Fatal("stale prompt retained a prior server closure after reconnect")
	}
	if _, ok := registry.Resolve("help"); !ok {
		t.Fatal("unrelated slash command was removed with MCP prompt source")
	}
}

func TestRegisterLiveMCPPromptsIgnoresSupersededServer(t *testing.T) {
	registry := slash.NewRegistry()
	registry.Register(slash.Cmd{Name: "mcp__resources__current", Source: "mcp:resources"})
	old := lazyLifecycleServer("resources")
	current := lazyLifecycleServer("resources")
	rt := &runtime{mcpServers: []*mcptools.Server{current}}

	rt.registerLiveMCPPrompts(context.Background(), registry, old)

	if _, ok := registry.Resolve("mcp__resources__current"); !ok {
		t.Fatal("superseded prompt discovery removed the current server prompts")
	}
}

func TestScheduleMCPPromptsAfterLaunchIncludesResourceOnlyServer(t *testing.T) {
	registry := slash.NewRegistry()
	registry.Register(slash.Cmd{Name: "mcp__resources__stale", Source: "mcp:resources"})
	server := lazyLifecycleServer("resources")
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := &runtime{
		mcpServers:      []*mcptools.Server{server},
		mcpLauncherDone: done,
		mcpPromptCtx:    ctx,
	}

	rt.scheduleMCPPromptsAfterLaunch(registry)
	// The startup reconciliation must wait for the launcher ownership boundary.
	if _, ok := registry.Resolve("mcp__resources__stale"); !ok {
		t.Fatal("prompt reconciliation ran before launcher completion")
	}
	close(done)
	completed := make(chan struct{})
	go func() {
		rt.mcpPromptWG.Wait()
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("prompt reconciliation did not finish after launcher completion")
	}
	if _, ok := registry.Resolve("mcp__resources__stale"); ok {
		t.Fatal("resource-only server was omitted from post-launch prompt reconciliation")
	}
}
