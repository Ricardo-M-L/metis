package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
)

func TestAutomationProcessInheritsDesktopAgentBudget(t *testing.T) {
	const slotKey = "METIS_DESKTOP_SUBAGENT_SLOT_DIR"
	const totalKey = "METIS_DESKTOP_SUBAGENT_SLOTS"
	const capKey = "METIS_DESKTOP_SUBAGENT_CAP"
	for _, desktop := range []bool{true, false} {
		name := "ordinary-cli"
		if desktop {
			name = "desktop"
		}
		t.Run(name, func(t *testing.T) {
			for _, key := range []string{slotKey, totalKey, capKey} {
				t.Setenv(key, "")
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			options := AutomationOptions{Root: filepath.Join(t.TempDir(), "cron"), WorkDir: t.TempDir(), Executable: binary}
			binding := RuntimeBindings{Automations: &options}
			if desktop {
				// Explicit Desktop composition must replace stale inherited limits.
				t.Setenv(slotKey, filepath.Join(t.TempDir(), "stale-slots"))
				t.Setenv(totalKey, "99")
				t.Setenv(capKey, "99")
				binding.IsolatedTurns = &IsolatedTurnOptions{
					Executable: binary, MaxParallel: 8, MaxConfigurableParallelism: 16,
					SubagentSlotDir: t.TempDir(), MaxTotalAgentSlots: 24, MaxSubagentsPerRoot: 6,
				}
			}
			s := NewServer("127.0.0.1:0", nil, nil, binding)
			manager := s.automations
			if manager == nil {
				t.Fatal("automation manager was not composed")
			}
			want := map[string]string{}
			if desktop {
				front, ok := s.isolatedRunner.(*processIsolatedTurnRunner)
				if !ok {
					t.Fatalf("foreground runner = %T", s.isolatedRunner)
				}
				total, perRoot := front.agentLimits()
				want = map[string]string{slotKey: front.subagentSlotDir, totalKey: strconv.Itoa(total), capKey: strconv.Itoa(perRoot)}
				if want[slotKey] != binding.IsolatedTurns.SubagentSlotDir || total != 24 || perRoot != 6 {
					t.Fatalf("foreground budget = %v", want)
				}
			}
			// Use a real short-lived process without invoking the user's CLI or a
			// provider. startProcessLocked still builds and starts its actual Env.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			manager.command = func(executable string, args ...string) *exec.Cmd {
				if executable != binary || strings.Join(args, " ") != "cron start" {
					t.Fatalf("unexpected automation command: %q %q", executable, args)
				}
				return exec.CommandContext(ctx, binary, "-test.run=^$")
			}
			manager.mu.Lock()
			process, err := manager.startProcessLocked([]string{"cron", "start"}, "")
			manager.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if err := process.cmd.Wait(); err != nil {
				t.Fatalf("automation fixture: %v: %s", err, process.output.String())
			}
			for _, key := range []string{slotKey, totalKey, capKey} {
				var values []string
				for _, entry := range process.cmd.Env {
					if strings.HasPrefix(entry, key+"=") {
						values = append(values, strings.TrimPrefix(entry, key+"="))
					}
				}
				if desktop {
					if len(values) != 1 || values[0] != want[key] {
						t.Fatalf("automation %s = %q, want foreground value %q once", key, values, want[key])
					}
				} else if len(values) != 0 {
					t.Fatalf("ordinary CLI automation injected %s = %q", key, values)
				}
			}
		})
	}
}

func automationBudgetServer(t *testing.T) (*Server, *resizablePreferenceRunner) {
	t.Helper()
	s, _ := testServer(t)
	runner := &resizablePreferenceRunner{total: 16, perRoot: 8}
	s.isolatedRunner = runner
	s.turnCoordinator = NewTurnCoordinator(8)
	s.maxTurnParallelism = MaxDesktopRootTurnParallelism
	s.maxTotalAgentSlots, s.maxSubagentsPerRoot = 16, 8
	s.desktopAgentSlotDir = t.TempDir()
	s.automations = &automationManager{
		options: AutomationOptions{DesktopSlotDir: s.desktopAgentSlotDir, TotalAgentSlots: 16, SubagentsPerRoot: 8},
		manual:  make(map[string]*automationProcess),
	}
	return s, runner
}

func assertAutomationBudget(t *testing.T, s *Server, runner *resizablePreferenceRunner, root, total, perRoot int) {
	t.Helper()
	if s.turnCoordinator.MaxParallel() != root || runner.total != total || runner.perRoot != perRoot ||
		s.maxTotalAgentSlots != total || s.maxSubagentsPerRoot != perRoot ||
		s.automations.options.TotalAgentSlots != total || s.automations.options.SubagentsPerRoot != perRoot {
		t.Fatalf("budget mismatch: root=%d runner=%+v server=%d/%d automation=%+v", s.turnCoordinator.MaxParallel(), runner, s.maxTotalAgentSlots, s.maxSubagentsPerRoot, s.automations.options)
	}
}

func TestAutomationBudgetJobHelper(t *testing.T) {
	if os.Getenv("METIS_TEST_AUTOMATION_BUDGET_JOB") != "1" {
		return
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestAutomationBudgetDefersRunningAndPendingNotificationJobs(t *testing.T) {
	s, runner := automationBudgetServer(t)
	registry := jobs.NewRegistry(t.TempDir())
	s.loop = &agent.Loop{Jobs: registry}
	t.Cleanup(func() { registry.ResetAndWait(time.Second) })
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestAutomationBudgetJobHelper$")
	cmd.Env = append(os.Environ(), "METIS_TEST_AUTOMATION_BUDGET_JOB=1", "GORACE=atexit_sleep_ms=0")
	release, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer release.Close()
	if _, err := registry.Spawn(jobs.SpawnArgs{Command: "background budget fixture", Cmd: cmd, Cancel: cancel}); err != nil {
		t.Fatal(err)
	}
	if !registry.HasPendingWork() {
		t.Fatal("fixture job is not running")
	}
	if s.applyDesktopParallelism(10, 24, 6) {
		t.Fatal("running background job allowed hot budget update")
	}
	assertAutomationBudget(t, s, runner, 8, 16, 8)
	if err := release.Close(); err != nil {
		t.Fatal(err)
	}
	waitBackgroundCondition(t, "published background completion", registry.HasPendingNotifications)
	if registry.HasPendingWork() {
		t.Fatal("fixture did not reach published, undrained notification state")
	}
	if s.applyDesktopParallelism(10, 24, 6) {
		t.Fatal("pending background notification allowed hot budget update")
	}
	assertAutomationBudget(t, s, runner, 8, 16, 8)
	<-registry.Notify()
	// A watcher that has no remaining work may stay armed indefinitely. Its
	// presence alone must not prevent an otherwise idle settings change.
	watchCtx, stopWatch := context.WithCancel(context.Background())
	s.backgroundGeneration, s.backgroundSession, s.backgroundCancel = 1, "idle", stopWatch
	watchDone := make(chan struct{})
	go func() { defer close(watchDone); s.watchBackgroundContinuation(watchCtx, "idle", 1, registry) }()
	defer func() { stopWatch(); <-watchDone }()
	if !s.applyDesktopParallelism(10, 24, 6) {
		t.Fatal("idle background watcher blocked budget update")
	}
	assertAutomationBudget(t, s, runner, 10, 24, 6)
}

func TestAutomationBudgetRunLockDefersWithoutBlocking(t *testing.T) {
	s, runner := automationBudgetServer(t)
	s.runMu.Lock()
	result := make(chan bool, 1)
	go func() { result <- s.applyDesktopParallelism(10, 24, 6) }()
	select {
	case applied := <-result:
		s.runMu.Unlock()
		if applied {
			t.Fatal("owned run lock allowed hot budget update")
		}
	case <-time.After(250 * time.Millisecond):
		s.runMu.Unlock()
		<-result
		t.Fatal("budget update blocked waiting for run lock")
	}
	assertAutomationBudget(t, s, runner, 8, 16, 8)
	if !s.applyDesktopParallelism(10, 24, 6) {
		t.Fatal("released run lock still blocked budget update")
	}
	assertAutomationBudget(t, s, runner, 10, 24, 6)
}

func TestAutomationProcessesDeferDesktopParallelismUntilIdle(t *testing.T) {
	for _, active := range []string{"scheduler", "manual"} {
		t.Run(active, func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			s, _ := testServer(t)
			runner := &resizablePreferenceRunner{total: 16, perRoot: 8}
			s.isolatedRunner = runner
			s.turnCoordinator = NewTurnCoordinator(8)
			s.maxTurnParallelism = MaxDesktopRootTurnParallelism
			s.maxTotalAgentSlots, s.maxSubagentsPerRoot = 16, 8
			s.desktopAgentSlotDir = t.TempDir()
			manager := &automationManager{
				options: AutomationOptions{DesktopSlotDir: s.desktopAgentSlotDir, TotalAgentSlots: 16, SubagentsPerRoot: 8},
				manual:  make(map[string]*automationProcess),
			}
			s.automations = manager
			if active == "scheduler" {
				manager.scheduler = &automationProcess{}
			} else {
				manager.manual["job-a"] = &automationProcess{}
			}
			const body = `{"rootTurnParallelism":10,"totalAgentParallelism":24,"subagentParallelism":6}`
			checkSaved := func(wantApplied bool) {
				t.Helper()
				response := automationRequest(s, http.MethodPost, "/api/preferences", body)
				var result struct {
					desktopPreferences
					Applied *bool `json:"parallelismApplied"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				// false is the API contract used by the settings UI to show the
				// saved/restart notice instead of claiming a live resize.
				if response.Code != http.StatusOK || result.Applied == nil || *result.Applied != wantApplied ||
					result.RootTurnParallelism != 10 || result.TotalAgentParallelism != 24 || result.SubagentParallelism != 6 {
					t.Fatalf("saved preference response = %d %s", response.Code, response.Body.String())
				}
			}
			checkSaved(false)
			if s.turnCoordinator.MaxParallel() != 8 || runner.total != 16 || runner.perRoot != 8 ||
				s.maxTotalAgentSlots != 16 || s.maxSubagentsPerRoot != 8 ||
				manager.options.TotalAgentSlots != 16 || manager.options.SubagentsPerRoot != 8 {
				t.Fatalf("%s changed live budgets: root=%d runner=%+v server=%d/%d automation=%+v", active, s.turnCoordinator.MaxParallel(), runner, s.maxTotalAgentSlots, s.maxSubagentsPerRoot, manager.options)
			}
			manager.mu.Lock()
			manager.scheduler = nil
			delete(manager.manual, "job-a")
			manager.mu.Unlock()
			checkSaved(true)
			if s.turnCoordinator.MaxParallel() != 10 || runner.total != 24 || runner.perRoot != 6 ||
				s.maxTotalAgentSlots != 24 || s.maxSubagentsPerRoot != 6 ||
				manager.options.TotalAgentSlots != 24 || manager.options.SubagentsPerRoot != 6 || manager.options.DesktopSlotDir != s.desktopAgentSlotDir {
				t.Fatalf("idle budgets diverged: root=%d runner=%+v server=%d/%d automation=%+v", s.turnCoordinator.MaxParallel(), runner, s.maxTotalAgentSlots, s.maxSubagentsPerRoot, manager.options)
			}
		})
	}
}
