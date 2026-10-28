//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
	bashbuiltin "github.com/Ricardo-M-L/metis/internal/tools/builtin/bash"
)

func TestRuntimePlanMarkerWriterHelper(t *testing.T) {
	path := os.Getenv("METIS_TEST_PLAN_MARKER_WRITER")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(2)
	}
	for {
		if _, err := file.Write([]byte("x")); err != nil {
			os.Exit(3)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitPlanMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("owned marker writer did not start")
}

func assertPlanMarkerStopped(t *testing.T, path string) {
	t.Helper()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("marker continued changing after Plan entry returned")
	}
}

func enterPlanWithDispatchLease(t *testing.T, rt *runtime) {
	t.Helper()
	release, ok, reason := rt.gate.TryAcquireToolDispatchLease()
	if !ok {
		t.Fatal(reason)
	}
	defer release()
	done := make(chan error, 1)
	go func() {
		result, err := builtin.NewEnterPlanModeWithGate(rt.gate).Execute(agent.WithPlanController(context.Background(), rt.loop), nil)
		if err == nil && result != nil && result.IsError {
			err = fmt.Errorf("enter plan rejected: %s", result.Output)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("EnterPlanMode deadlocked while draining resources under its shared dispatch lease")
	}
}

func TestRuntimePlanEntryStopsOwnedJobWritersFromEveryMode(t *testing.T) {
	for _, from := range []permission.Mode{permission.ModeDefault, permission.ModeAcceptEdits, permission.ModeDontAsk, permission.ModeBypassPermissions} {
		t.Run(string(from), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "marker")
			pool := jobs.NewRegistry(t.TempDir())
			t.Cleanup(func() { pool.ResetAndWait(0) })
			cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimePlanMarkerWriterHelper$")
			cmd.Env = append(os.Environ(), "METIS_TEST_PLAN_MARKER_WRITER="+marker)
			if _, err := pool.Spawn(jobs.SpawnArgs{Command: "owned-plan-marker-writer", Cmd: cmd}); err != nil {
				t.Fatal(err)
			}
			waitPlanMarker(t, marker)
			gate := permission.New(from)
			loop := &agent.Loop{Gate: gate, Jobs: pool}
			rt := &runtime{gate: gate, loop: loop, permissionMode: from}
			installRuntimePermissionListener(rt)
			enterPlanWithDispatchLease(t, rt)
			if gate.Mode() != permission.ModePlan || !loop.IsPlanMode() {
				t.Fatal("Plan was not active after writer drain")
			}
			if len(pool.List()) != 0 || cmd.ProcessState == nil {
				t.Fatal("Plan returned before old jobs were joined")
			}
			assertPlanMarkerStopped(t, marker)
			if d, _ := gate.Check(context.Background(), "Read", marker); d != permission.DecisionAllow {
				t.Fatal("read-only exploration was lost")
			}
		})
	}
}

func TestRuntimePlanEntryJoinsBackgroundAgentWriter(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	marker := filepath.Join(t.TempDir(), "agent-marker")
	gate := permission.New(permission.ModeAcceptEdits)
	gate.AppendRules(permission.Rule{Tool: "Bash", Verb: permission.DecisionAllow, Source: "test"})
	roster := agent.NewRoster(0)
	pool := jobs.NewRegistry(t.TempDir())
	t.Cleanup(func() { _ = roster.CancelAndWait(context.Background()); pool.ResetAndWait(0) })
	registry := tools.NewRegistry()
	registry.Register(bashbuiltin.New(gate, config.ToolBashSettings{Shell: "/bin/bash", TimeoutSeconds: 600, MaxOutputBytes: 1024}))
	bashbuiltin.AttachJobsRegistry(registry, pool, gate)
	loop := &agent.Loop{Gate: gate, Jobs: pool}
	rt := &runtime{gate: gate, loop: loop, registry: registry, subAgentRoster: roster, permissionMode: permission.ModeAcceptEdits}
	installRuntimePermissionListener(rt)
	provider := &permissionBoundaryBashProvider{command: fmt.Sprintf(`while :; do printf x >> %q; sleep 0.01; done`, marker)}
	tool := builtin.NewAgent(gate, provider, registry, "model", "system").WithRoster(roster).WithJobsPool(pool)
	result, err := tool.Execute(context.Background(), map[string]any{"prompt": "run the owned marker writer", "name": "plan-marker-child", "run_in_background": true})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("start owned child: %+v %v", result, err)
	}
	// The write runs inside the child's foreground Bash under its own shared
	// dispatch lease; the root enters Plan while that real process is alive.
	waitPlanMarker(t, marker)
	enterPlanWithDispatchLease(t, rt)
	if roster.Count() != 0 {
		t.Fatal("Plan returned with an old background Agent still live")
	}
	assertPlanMarkerStopped(t, marker)
}
