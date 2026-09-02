//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
	bashbuiltin "github.com/Ricardo-M-L/metis/internal/tools/builtin/bash"
)

type permissionBoundaryBashStream struct {
	events []llm.StreamEvent
	index  int
}

func (s *permissionBoundaryBashStream) Recv() (llm.StreamEvent, error) {
	if s.index >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (*permissionBoundaryBashStream) Close() error { return nil }

type permissionBoundaryBlockingStream struct {
	ctx context.Context
}

func (s *permissionBoundaryBlockingStream) Recv() (llm.StreamEvent, error) {
	<-s.ctx.Done()
	return llm.StreamEvent{}, s.ctx.Err()
}

func (*permissionBoundaryBlockingStream) Close() error { return nil }

// permissionBoundaryBashProvider drives a real child Loop into one foreground
// Bash call. While Bash is live, the child's dispatcher owns its cloned Gate's
// read lease; the second provider call only keeps the loop alive if execution
// happens to return before lifecycle cancellation lands.
type permissionBoundaryBashProvider struct {
	command string
	calls   atomic.Int32
}

func (*permissionBoundaryBashProvider) Name() string          { return "permission-boundary-bash" }
func (*permissionBoundaryBashProvider) ModelID() string       { return "permission-boundary-bash" }
func (*permissionBoundaryBashProvider) MaxContextTokens() int { return 100_000 }
func (*permissionBoundaryBashProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("permission boundary provider only supports Stream")
}

func (p *permissionBoundaryBashProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	if p.calls.Add(1) != 1 {
		return &permissionBoundaryBlockingStream{ctx: ctx}, nil
	}
	input, err := json.Marshal(map[string]any{
		"command":           p.command,
		"description":       "hold a foreground process tree across permission revoke",
		"run_in_background": false,
	})
	if err != nil {
		return nil, err
	}
	return &permissionBoundaryBashStream{events: []llm.StreamEvent{
		{Type: "tool_use_start", ToolUseID: "permission-boundary-bash", ToolName: "Bash"},
		{Type: "tool_input_delta", ToolUseID: "permission-boundary-bash", InputDelta: string(input)},
		{Type: "tool_use_stop", ToolUseID: "permission-boundary-bash"},
		{Type: "message_delta", StopReason: "tool_use"},
		{Type: "message_stop"},
	}}, nil
}

func TestRuntimeFullAccessExitJoinsBackgroundAgentForegroundBash(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("METIS_HOME", tmp)
	leaderPath := filepath.Join(tmp, "permission-boundary-leader.pid")
	childPath := filepath.Join(tmp, "permission-boundary-child.pid")
	command := fmt.Sprintf(
		`printf '%%s' "$$" > %q; tail -f /dev/null & child=$!; printf '%%s' "$child" > %q; wait`,
		leaderPath, childPath,
	)

	sandboxRoot := filepath.Join(tmp, "sandbox")
	if err := os.MkdirAll(sandboxRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{
		Mode:     string(sandbox.ModePermissions),
		TempRoot: sandboxRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	gate := permission.New(permission.ModeDefault)
	roster := agent.NewRoster(0)
	rootJobs := jobs.NewRegistry(tmp)
	registry := tools.NewRegistry()
	registry.Register(bashbuiltin.New(gate, config.ToolBashSettings{
		Shell:          "/bin/bash",
		TimeoutSeconds: 600,
		MaxOutputBytes: 16 * 1024,
	}))
	bashbuiltin.AttachJobsRegistry(registry, rootJobs, gate)
	loop := &agent.Loop{Jobs: rootJobs}
	rt := &runtime{
		gate:           gate,
		loop:           loop,
		sandbox:        manager,
		registry:       registry,
		subAgentRoster: roster,
		permissionMode: permission.ModeDefault,
	}
	installRuntimePermissionListener(rt)
	if err := applyRuntimePermissionMode(rt, permission.ModeFullAccess); err != nil {
		t.Fatalf("enter fullAccess: %v", err)
	}

	server := lazyLifecycleServer("childgate", "unsafe")
	t.Cleanup(func() { _ = server.Close() })
	if !rt.adoptMCPServer(server, server.Tools(), true) {
		t.Fatal("adopt fullAccess MCP server")
	}

	provider := &permissionBoundaryBashProvider{command: command}
	agentTool := builtin.NewAgent(gate, provider, registry, "test-model", "test-system").
		WithRoster(roster).
		WithJobsPool(rootJobs)
	result, err := agentTool.Execute(context.Background(), map[string]any{
		"prompt":            "start the foreground process and keep working",
		"name":              "permission-boundary-child",
		"run_in_background": true,
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("background Agent spawn = (%+v, %v)", result, err)
	}

	leaderPID := waitForPermissionBoundaryPID(t, leaderPath, 5*time.Second)
	childPID := waitForPermissionBoundaryPID(t, childPath, 5*time.Second)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = roster.CancelAndWait(cleanupCtx)
		cancel()
		rootJobs.ResetAndWait(0)
		killPermissionBoundaryProcess(leaderPID)
		killPermissionBoundaryProcess(childPID)
	})
	if !permissionBoundaryProcessExists(leaderPID) || !permissionBoundaryProcessExists(childPID) {
		t.Fatalf("process tree was not live before transition: leader=%d child=%d", leaderPID, childPID)
	}

	transitionDone := make(chan error, 1)
	go func() { transitionDone <- applyRuntimePermissionMode(rt, permission.ModeDefault) }()
	select {
	case err := <-transitionDone:
		if err != nil {
			t.Fatalf("leave fullAccess: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fullAccess exit deadlocked behind the child foreground Bash lease")
	}

	waitForPermissionBoundaryExit(t, leaderPID, 3*time.Second)
	waitForPermissionBoundaryExit(t, childPID, 3*time.Second)
	if got := roster.Count(); got != 0 {
		t.Fatalf("roster count after fullAccess revoke = %d, want 0", got)
	}
	if got := len(rootJobs.List()); got != 0 {
		t.Fatalf("root jobs after fullAccess revoke = %d, want 0", got)
	}
	state := manager.State()
	if gate.Mode() != permission.ModeDefault || state.FullAccessRequired || state.Effective != sandbox.ModePermissions {
		t.Fatalf("safe posture after transition: gate=%s sandbox=%+v", gate.Mode(), state)
	}
	if len(rt.mcpServers) != 0 {
		t.Fatalf("fullAccess MCP retained after transition: %#v", rt.mcpServers)
	}
	if _, ok := registry.Get("mcp__childgate__unsafe"); ok {
		t.Fatal("fullAccess MCP namespace retained after transition")
	}
}

func waitForPermissionBoundaryPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PID file %s was not populated within %s", path, timeout)
	return 0
}

func permissionBoundaryProcessExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func waitForPermissionBoundaryExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !permissionBoundaryProcessExists(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still exists after %s", pid, timeout)
}

func killPermissionBoundaryProcess(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
