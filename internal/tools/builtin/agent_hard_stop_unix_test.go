//go:build !windows

package builtin

import (
	"context"
	"fmt"
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
	"github.com/Ricardo-M-L/metis/internal/tools"
	bashbuiltin "github.com/Ricardo-M-L/metis/internal/tools/builtin/bash"
)

// hardStopBashProvider asks for one real Bash invocation, then waits for the
// child lifecycle to be cancelled. It exercises the complete model -> Agent ->
// dispatcher -> Bash path rather than calling the process helper directly.
type hardStopBashProvider struct {
	command    string
	background bool
	calls      atomic.Int32
}

func (*hardStopBashProvider) Name() string          { return "hard-stop-bash" }
func (*hardStopBashProvider) MaxContextTokens() int { return 100_000 }
func (*hardStopBashProvider) ModelID() string       { return "hard-stop-bash" }
func (*hardStopBashProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, fmt.Errorf("not implemented")
}

func (p *hardStopBashProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	if p.calls.Add(1) == 1 {
		input := fmt.Sprintf(`{"command":%q,"description":"hard-stop process tree","run_in_background":%t}`, p.command, p.background)
		return &fakeStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: "bash-hard-stop", ToolName: "Bash"},
			{Type: "tool_input_delta", ToolUseID: "bash-hard-stop", InputDelta: input},
			{Type: "tool_use_stop", ToolUseID: "bash-hard-stop", InputDelta: input},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}}, nil
	}
	return &blockingStream{ctx: ctx}, nil
}

func TestBackgroundAgentCancelAndWaitReapsBashProcessTree(t *testing.T) {
	for _, explicitBackground := range []bool{false, true} {
		t.Run(fmt.Sprintf("bash_background_%t", explicitBackground), func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("METIS_HOME", tmp)
			leaderPath := filepath.Join(tmp, "leader.pid")
			childPath := filepath.Join(tmp, "child.pid")
			// tail keeps a real descendant alive without triggering Bash's
			// deliberate-sleep detector, so false covers foreground InterruptBlock.
			command := fmt.Sprintf(
				`printf '%%s' "$$" > %q; tail -f /dev/null & child=$!; printf '%%s' "$child" > %q; wait`,
				leaderPath, childPath,
			)

			gate := permission.New(permission.ModeFullAccess)
			registry := tools.NewRegistry()
			registry.Register(bashbuiltin.New(gate, config.ToolBashSettings{
				Shell:          "/bin/bash",
				TimeoutSeconds: 600,
				MaxOutputBytes: 16 * 1024,
			}))
			roster := agent.NewRoster(0)
			rootJobs := jobs.NewRegistry(tmp)
			bashbuiltin.AttachJobsRegistry(registry, rootJobs, gate)
			provider := &hardStopBashProvider{command: command, background: explicitBackground}
			agentTool := NewAgent(gate, provider, registry, "test-model", "system").
				WithRoster(roster).
				WithJobsPool(rootJobs)
			t.Cleanup(func() {
				// Every fatal path must still stop the detached runner/process tree.
				cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Second)
				_ = roster.CancelAndWait(cleanupCtx)
				cancelCleanup()
				if pid := readPIDFile(leaderPath); pid > 0 {
					_ = syscall.Kill(-pid, syscall.SIGKILL)
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				if pid := readPIDFile(childPath); pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				rootJobs.ResetAndWait(0)
			})

			res, err := agentTool.Execute(context.Background(), map[string]any{
				"prompt":            "start the process and keep working",
				"run_in_background": true,
			})
			if err != nil || res == nil || res.IsError {
				t.Fatalf("background Agent spawn = (%+v, %v)", res, err)
			}

			leaderPID := waitForPIDFile(t, leaderPath, 5*time.Second)
			childPID := waitForPIDFile(t, childPath, 5*time.Second)
			if !processExists(leaderPID) || !processExists(childPID) {
				t.Fatalf("process tree was not live before cancellation: leader=%d child=%d", leaderPID, childPID)
			}

			joinCtx, cancelJoin := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelJoin()
			started := time.Now()
			if err := roster.CancelAndWait(joinCtx); err != nil {
				t.Fatalf("CancelAndWait: %v", err)
			}
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Fatalf("CancelAndWait took %s; hard-stop boundary did not terminate promptly", elapsed)
			}
			waitForProcessExit(t, leaderPID, 3*time.Second)
			waitForProcessExit(t, childPID, 3*time.Second)
			if got := roster.Count(); got != 0 {
				t.Fatalf("roster count after joined cancellation = %d, want 0", got)
			}
			if got := len(rootJobs.List()); got != 0 {
				t.Fatalf("root jobs unexpectedly retained child-owned work: %d", got)
			}
		})
	}
}

func TestAgentParentDeadlineReapsInterruptBlockBashProcessTree(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("agent_background_%t", background), func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("METIS_HOME", tmp)
			leaderPath := filepath.Join(tmp, "leader.pid")
			childPath := filepath.Join(tmp, "child.pid")
			command := fmt.Sprintf(
				`printf '%%s' "$$" > %q; tail -f /dev/null & child=$!; printf '%%s' "$child" > %q; wait`,
				leaderPath, childPath,
			)
			gate := permission.New(permission.ModeFullAccess)
			registry := tools.NewRegistry()
			registry.Register(bashbuiltin.New(gate, config.ToolBashSettings{
				Shell:          "/bin/bash",
				TimeoutSeconds: 600,
				MaxOutputBytes: 16 * 1024,
			}))
			roster := agent.NewRoster(0)
			rootJobs := jobs.NewRegistry(tmp)
			bashbuiltin.AttachJobsRegistry(registry, rootJobs, gate)
			agentTool := NewAgent(gate, &hardStopBashProvider{command: command}, registry, "model", "system").
				WithRoster(roster).
				WithJobsPool(rootJobs)
			t.Cleanup(func() {
				cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Second)
				_ = roster.CancelAndWait(cleanupCtx)
				cancelCleanup()
				if pid := readPIDFile(leaderPath); pid > 0 {
					_ = syscall.Kill(-pid, syscall.SIGKILL)
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				if pid := readPIDFile(childPath); pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				rootJobs.ResetAndWait(0)
			})
			parentCtx, cancelParent := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelParent()
			executed := make(chan struct{})
			go func() {
				defer close(executed)
				result, err := agentTool.Execute(parentCtx, map[string]any{
					"prompt":            "run the command until the shared deadline",
					"run_in_background": background,
					"timeout_seconds":   3600,
				})
				if err != nil || result == nil || result.IsError == background {
					t.Errorf("Execute = (%+v, %v), background=%t", result, err, background)
				}
			}()
			leaderPID := waitForPIDFile(t, leaderPath, time.Second)
			childPID := waitForPIDFile(t, childPath, time.Second)
			// Ordinary turn cancellation must not interrupt the running Bash.
			// The original parent deadline must still reap its full process tree.
			cancelParent()
			time.Sleep(50 * time.Millisecond)
			if !processExists(leaderPID) || !processExists(childPID) {
				t.Fatal("ordinary parent cancellation interrupted the Bash process tree")
			}
			waitForProcessExit(t, leaderPID, 4*time.Second)
			waitForProcessExit(t, childPID, time.Second)
			waitForRosterCount(t, roster, 0, time.Second)
			select {
			case <-executed:
			case <-time.After(time.Second):
				t.Fatal("Agent did not return after its process tree exited")
			}
		})
	}
}

func waitForPIDFile(t *testing.T, path string, timeout time.Duration) int {
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

func readPIDFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func waitForProcessExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still exists after %s", pid, timeout)
}
