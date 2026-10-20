package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

type coordinatorAdapterTool struct {
	pubtool.BaseTool
	name     string
	readOnly bool
}

func (t coordinatorAdapterTool) Name() string                { return t.name }
func (t coordinatorAdapterTool) Description() string         { return "adapter test tool" }
func (t coordinatorAdapterTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (t coordinatorAdapterTool) Concurrency(map[string]any) pubtool.Concurrency {
	return pubtool.ConcurrencySafe
}
func (t coordinatorAdapterTool) IsReadOnly(map[string]any) bool { return t.readOnly }
func (t coordinatorAdapterTool) CanUse(context.Context, map[string]any) (pubtool.Permission, string) {
	return pubtool.PermissionAllow, ""
}
func (t coordinatorAdapterTool) Execute(context.Context, map[string]any) (*pubtool.Result, error) {
	return &pubtool.Result{Output: "ok"}, nil
}

type coordinatorAdapterFixture struct {
	store *projectcoord.Store
	run   projectcoord.Run
	item  projectcoord.WorkItem
	ctx   context.Context
	guard pubtool.OperationExecutor
}

func newCoordinatorAdapterFixture(t *testing.T) coordinatorAdapterFixture {
	t.Helper()
	store := projectcoord.NewStore(filepath.Join(t.TempDir(), "state"))
	run, err := store.Create(projectcoord.CreateInput{Workspace: t.TempDir(), Goal: "Guard remote writes", Items: []projectcoord.WorkItemInput{{ID: "send", Phase: projectcoord.PhaseCustom, Subject: "Send exactly one request", Prompt: "Send one request", MaxAttempts: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "guard-test", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := coordinatorOperationContext(context.Background(), store, run.Workspace, run.ID, item, item.Owner)
	t.Cleanup(func() { cancel(nil) })
	return coordinatorAdapterFixture{store: store, run: run, item: item, ctx: ctx, guard: pubtool.OperationExecutorFromContext(ctx)}
}

func (f coordinatorAdapterFixture) latest(t *testing.T) projectcoord.Run {
	t.Helper()
	run, err := f.store.Get(f.run.Workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestCoordinatorOperationAdapterReplaysCompleteResultAcrossClaims(t *testing.T) {
	f := newCoordinatorAdapterFixture(t)
	tool := coordinatorAdapterTool{name: "SendMessage"}
	input := map[string]any{"channel": "test:target", "text": "only once"}
	var calls atomic.Int32
	want := &pubtool.Result{Output: "remote receipt", Display: "sent", Meta: map[string]any{"receipt": "receipt-123", "numeric_receipt": int64(9007199254740993)}, Presentation: map[string]any{"kind": "receipt"}, Images: []pubtool.ImageAttachment{{MediaType: "image/png", Data: "cG5n"}}}
	invoke := func(context.Context) (*pubtool.Result, error) { calls.Add(1); return want, nil }
	first, err := f.guard.Execute(f.ctx, tool, input, invoke)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Fail(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, projectcoord.Failure{Code: projectcoord.FailureWorkerInterrupted, Summary: "Interrupted after confirmed send"}, ""); err != nil {
		t.Fatal(err)
	}
	item, err := f.store.ClaimNext(f.run.Workspace, f.run.ID, projectcoord.ClaimOptions{Worker: "guard-test-2", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := coordinatorOperationContext(context.Background(), f.store, f.run.Workspace, f.run.ID, item, item.Owner)
	defer cancel(nil)
	second, err := pubtool.OperationExecutorFromContext(ctx).Execute(ctx, tool, input, invoke)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if calls.Load() != 1 || string(a) != string(b) || len(f.latest(t).Operations) != 1 {
		t.Fatalf("confirmed remote write repeated or lost full result: calls=%d first=%s second=%s", calls.Load(), a, b)
	}
	if _, err := f.guard.Execute(f.ctx, tool, map[string]any{"text": "different"}, invoke); !errors.Is(err, projectcoord.ErrOperationUncertain) || calls.Load() != 1 {
		t.Fatalf("stale worker performed a new write: %v calls=%d", err, calls.Load())
	}
}

func TestCoordinatorOperationAdapterParallelIntentionalCallsHaveDistinctOccurrences(t *testing.T) {
	f := newCoordinatorAdapterFixture(t)
	var calls atomic.Int32
	var wait sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := f.guard.Execute(f.ctx, coordinatorAdapterTool{name: "remote_write"}, map[string]any{"id": "one"}, func(context.Context) (*pubtool.Result, error) {
				calls.Add(1)
				time.Sleep(5 * time.Millisecond)
				return &pubtool.Result{Output: "receipt"}, nil
			})
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	run := f.latest(t)
	if calls.Load() != 8 || len(run.Operations) != 8 {
		t.Fatalf("intentional calls were collapsed: calls=%d operations=%d", calls.Load(), len(run.Operations))
	}
	for index, operation := range run.Operations {
		if operation.Occurrence != index+1 || operation.Status != projectcoord.OperationCompleted {
			t.Fatalf("parallel logical actions lost their stable sequence: %+v", run.Operations)
		}
	}
}

func TestCoordinatorOperationAdapterIntentionalRepeatExecutesAndRecoveredSequenceReplays(t *testing.T) {
	f := newCoordinatorAdapterFixture(t)
	tool := coordinatorAdapterTool{name: "Bash"}
	input := map[string]any{"command": "run the same check again"}
	calls := 0
	invoke := func(context.Context) (*pubtool.Result, error) {
		calls++
		return &pubtool.Result{Output: fmt.Sprintf("real-check-%d", calls), Presentation: map[string]any{"kind": "receipt", "id": fmt.Sprint(calls)}, Images: []pubtool.ImageAttachment{{MediaType: "image/png", Data: "cG5n"}}}, nil
	}
	results := make([]string, 2)
	for index := range results {
		result, err := f.guard.Execute(f.ctx, tool, input, invoke)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(result)
		results[index] = string(encoded)
	}
	if calls != 2 || len(f.latest(t).Operations) != 2 {
		t.Fatal("ordinary same-claim repeated commands were cached")
	}
	if _, err := f.store.Fail(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, projectcoord.Failure{Code: projectcoord.FailureWorkerInterrupted, Summary: "Interrupted after both confirmed actions"}, ""); err != nil {
		t.Fatal(err)
	}
	item, err := f.store.ClaimNext(f.run.Workspace, f.run.ID, projectcoord.ClaimOptions{Worker: "recovered-worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := coordinatorOperationContext(context.Background(), f.store, f.run.Workspace, f.run.ID, item, item.Owner)
	defer cancel(nil)
	guard := pubtool.OperationExecutorFromContext(ctx)
	for index, original := range results {
		result, err := guard.Execute(ctx, tool, input, invoke)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(result)
		if string(encoded) != original || calls != 2 {
			t.Fatalf("recovered occurrence %d resent the action or lost its full result: calls=%d", index+1, calls)
		}
	}
	if _, err := guard.Execute(ctx, tool, input, invoke); err != nil || calls != 3 || len(f.latest(t).Operations) != 3 {
		t.Fatalf("new intentional occurrence after replay could not execute: calls=%d err=%v", calls, err)
	}
}

func TestCoordinatorOperationCodePresentationAndTimeoutChangesReplayAcrossClaims(t *testing.T) {
	cases := []struct {
		toolName string
		original map[string]any
		recovery map[string]any
	}{
		{"Bash", map[string]any{"command": "send-one-request", "description": "Send the original request", "timeout_ms": 1000, "run_in_background": false, "await_completion": false, "required_for_completion": false, "env": map[string]any{"TARGET": "one"}}, map[string]any{"command": "send-one-request", "description": "Resume the request after restart", "timeout_ms": 600000, "env": map[string]any{"TARGET": "one"}}},
		{"RunCode", map[string]any{"code": "send_one_request()", "language": "python", "timeout": 30}, map[string]any{"code": "send_one_request()", "language": "python", "timeout": 300}},
	}
	for _, test := range cases {
		t.Run(test.toolName, func(t *testing.T) {
			f := newCoordinatorAdapterFixture(t)
			tool := coordinatorAdapterTool{name: test.toolName}
			calls := 0
			invoke := func(context.Context) (*pubtool.Result, error) {
				calls++
				return &pubtool.Result{Output: "external receipt"}, nil
			}
			originalJSON, _ := json.Marshal(test.original)
			if _, err := f.guard.Execute(f.ctx, tool, test.original, invoke); err != nil {
				t.Fatal(err)
			}
			afterJSON, _ := json.Marshal(test.original)
			if string(afterJSON) != string(originalJSON) {
				t.Fatal("operation hashing changed the real invocation's arguments")
			}
			if _, err := f.store.Fail(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, projectcoord.Failure{Code: projectcoord.FailureWorkerInterrupted, Summary: "Restart after confirmed receipt"}, ""); err != nil {
				t.Fatal(err)
			}
			item, err := f.store.ClaimNext(f.run.Workspace, f.run.ID, projectcoord.ClaimOptions{Worker: "restart", Lease: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := coordinatorOperationContext(context.Background(), f.store, f.run.Workspace, f.run.ID, item, item.Owner)
			defer cancel(nil)
			result, err := pubtool.OperationExecutorFromContext(ctx).Execute(ctx, tool, test.recovery, invoke)
			if err != nil || result.Output != "external receipt" || calls != 1 || len(f.latest(t).Operations) != 1 {
				t.Fatalf("metadata or timeout changes repeated a confirmed external action: result=%+v calls=%d err=%v", result, calls, err)
			}
		})
	}
}

func TestCanonicalOperationInputPreservesActionScopeAndUnknownTools(t *testing.T) {
	for _, toolName := range []string{"Bash", "RunCode", "RemoteAction"} {
		t.Run(toolName, func(t *testing.T) {
			input := map[string]any{"command": "command-bytes", "code": "code-bytes", "language": "python", "cwd": "/scope", "env": map[string]any{"TARGET": "one"}, "body": "request-body", "run_in_background": true, "description": "display text", "timeout_ms": 10, "timeout": 20}
			canonical := canonicalOperationInput(coordinatorAdapterTool{name: toolName}, input)
			for _, field := range []string{"command", "code", "language", "cwd", "env", "body", "run_in_background"} {
				actual, _ := json.Marshal(canonical[field])
				original, _ := json.Marshal(input[field])
				if string(actual) != string(original) {
					t.Fatalf("hash normalization lost action/scope field %s", field)
				}
			}
			if toolName == "RemoteAction" {
				actual, _ := json.Marshal(canonical)
				original, _ := json.Marshal(input)
				if string(actual) != string(original) {
					t.Fatal("unknown tool arguments were reinterpreted")
				}
			}
		})
	}
}

func TestCoordinatorOperationBashCommandAndEffectiveCwdRemainDistinct(t *testing.T) {
	f := newCoordinatorAdapterFixture(t)
	tool := coordinatorAdapterTool{name: "Bash"}
	calls := 0
	invoke := func(context.Context) (*pubtool.Result, error) {
		calls++
		return &pubtool.Result{Output: "receipt"}, nil
	}
	for _, call := range []struct{ cwd, command string }{{"/scope-one", "send-one"}, {"/scope-two", "send-one"}, {"/scope-one", "send-two"}} {
		ctx := agent.WithCwd(f.ctx, call.cwd)
		if _, err := f.guard.Execute(ctx, tool, map[string]any{"command": call.command, "description": "same description"}, invoke); err != nil {
			t.Fatal(err)
		}
	}
	run := f.latest(t)
	if calls != 3 || len(run.Operations) != 3 {
		t.Fatal("different command/scope actions were collapsed")
	}
	seen := make(map[string]bool)
	for _, op := range run.Operations {
		if op.Occurrence != 1 || seen[op.ID] {
			t.Fatalf("distinct action/scope did not get a distinct signature: %+v", run.Operations)
		}
		seen[op.ID] = true
	}
}

func TestCoordinatorOperationUnknownBashCannotBypassWithChangedCommandOrDescription(t *testing.T) {
	f := newCoordinatorAdapterFixture(t)
	tool := coordinatorAdapterTool{name: "Bash"}
	_, err := f.guard.Execute(f.ctx, tool, map[string]any{"command": "send-one", "description": "first request", "timeout_ms": 1000}, func(context.Context) (*pubtool.Result, error) {
		return nil, errors.New("request applied but connection closed")
	})
	if !errors.Is(err, projectcoord.ErrOperationUncertain) {
		t.Fatal(err)
	}
	calls := 0
	ctx, cancel := coordinatorOperationContext(context.Background(), f.store, f.run.Workspace, f.run.ID, f.item, f.item.Owner)
	defer cancel(nil)
	_, err = pubtool.OperationExecutorFromContext(ctx).Execute(ctx, tool, map[string]any{"command": "send-two", "description": "different request", "timeout_ms": 600000}, func(context.Context) (*pubtool.Result, error) { calls++; return &pubtool.Result{Output: "sent"}, nil })
	if !errors.Is(err, projectcoord.ErrOperationUncertain) || calls != 0 || len(f.latest(t).Operations) != 1 {
		t.Fatalf("changed arguments bypassed unknown external outcome: calls=%d err=%v", calls, err)
	}
}

func TestCoordinatorOperationExplicitBackgroundBashIsRejectedBeforeDispatch(t *testing.T) {
	for _, flag := range []string{"run_in_background", "await_completion", "required_for_completion"} {
		t.Run(flag, func(t *testing.T) {
			f := newCoordinatorAdapterFixture(t)
			calls := 0
			result, err := f.guard.Execute(f.ctx, coordinatorAdapterTool{name: "Bash"}, map[string]any{"command": "send-one", "description": "Send one request", flag: true}, func(context.Context) (*pubtool.Result, error) {
				calls++
				return &pubtool.Result{Output: "should not run"}, nil
			})
			if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, "not executed") || calls != 0 {
				t.Fatalf("background request was dispatched or marked ambiguous: result=%+v err=%v calls=%d", result, err, calls)
			}
			if len(f.latest(t).Operations) != 0 || f.ctx.Err() != nil {
				t.Fatal("safe pre-dispatch refusal created an uncertain journal or cancelled the worker")
			}
			// A corrected foreground request can proceed in the same claim.
			result, err = f.guard.Execute(f.ctx, coordinatorAdapterTool{name: "Bash"}, map[string]any{"command": "send-one", "description": "Send one request", flag: false}, func(context.Context) (*pubtool.Result, error) {
				calls++
				return &pubtool.Result{Output: "foreground receipt"}, nil
			})
			if err != nil || result.IsError || calls != 1 || len(f.latest(t).Operations) != 1 {
				t.Fatalf("safe refusal prevented corrected foreground work: result=%+v err=%v calls=%d", result, err, calls)
			}
		})
	}
}

func TestCoordinatorOperationAdapterErrorsHoldAllChangedArguments(t *testing.T) {
	cases := []struct {
		name   string
		invoke func(context.Context) (*pubtool.Result, error)
	}{
		{"error", func(context.Context) (*pubtool.Result, error) {
			return nil, errors.New("remote connection closed after request")
		}},
		{"is-error", func(context.Context) (*pubtool.Result, error) {
			return &pubtool.Result{IsError: true, Output: "response failed"}, nil
		}},
		{"nil-result", func(context.Context) (*pubtool.Result, error) { return nil, nil }},
		{"unserializable-result", func(context.Context) (*pubtool.Result, error) {
			return &pubtool.Result{Output: "sent", Meta: map[string]any{"not_json": make(chan int)}}, nil
		}},
		{"oversized-result", func(context.Context) (*pubtool.Result, error) {
			return &pubtool.Result{Output: strings.Repeat("x", projectcoord.MaxOperationResultBytes)}, nil
		}},
		{"async-job-start-only", func(context.Context) (*pubtool.Result, error) {
			return &pubtool.Result{Output: "job was started", Presentation: map[string]any{"kind": "background_job", "job_id": "unfinished-job"}}, nil
		}},
		{"deadline", func(ctx context.Context) (*pubtool.Result, error) {
			<-ctx.Done()
			return &pubtool.Result{Output: "possibly sent"}, nil
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newCoordinatorAdapterFixture(t)
			ctx := f.ctx
			if test.name == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			_, err := f.guard.Execute(ctx, coordinatorAdapterTool{name: "remote_write"}, map[string]any{"text": "first"}, test.invoke)
			if !errors.Is(err, projectcoord.ErrOperationUncertain) || !errors.Is(context.Cause(f.ctx), projectcoord.ErrOperationUncertain) {
				t.Fatalf("unknown external outcome did not stop the item: err=%v cause=%v", err, context.Cause(f.ctx))
			}
			run := f.latest(t)
			if len(run.Operations) != 1 || run.Operations[0].Status != projectcoord.OperationUncertain || run.Items[0].Failure == nil || run.Items[0].Failure.Code != projectcoord.FailureOperationUncertain {
				t.Fatalf("unknown side effect was treated as retryable: %+v", run)
			}
			calls := 0
			ctx2, cancel := coordinatorOperationContext(context.Background(), f.store, run.Workspace, run.ID, f.item, f.item.Owner)
			defer cancel(nil)
			_, err = pubtool.OperationExecutorFromContext(ctx2).Execute(ctx2, coordinatorAdapterTool{name: "other_remote_write"}, map[string]any{"text": "reformulated"}, func(context.Context) (*pubtool.Result, error) { calls++; return &pubtool.Result{Output: "sent"}, nil })
			if !errors.Is(err, projectcoord.ErrOperationUncertain) || calls != 0 {
				t.Fatalf("changed arguments bypassed an uncertain write: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestCoordinatorOperationAdapterDoesNotCacheReadsOrContainers(t *testing.T) {
	for _, name := range []string{"Read", "Agent", "Fork", "Ralph"} {
		t.Run(name, func(t *testing.T) {
			f := newCoordinatorAdapterFixture(t)
			calls := 0
			tool := coordinatorAdapterTool{name: name, readOnly: name == "Read"}
			for range 2 {
				_, err := f.guard.Execute(f.ctx, tool, map[string]any{"path": "same"}, func(ctx context.Context) (*pubtool.Result, error) {
					if pubtool.OperationExecutorFromContext(ctx) != f.guard {
						t.Fatal("execution container lost guard context")
					}
					calls++
					return &pubtool.Result{Output: fmt.Sprint(calls)}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if calls != 2 || len(f.latest(t).Operations) != 0 {
				t.Fatalf("read/container was cached: calls=%d", calls)
			}
		})
	}
}

func TestCoordinatorOperationAdapterGuardsOpaqueCodeEvenWhenClassifiedReadOnly(t *testing.T) {
	for _, name := range []string{"Bash", "RunCode"} {
		t.Run(name, func(t *testing.T) {
			f := newCoordinatorAdapterFixture(t)
			calls := 0
			for range 2 {
				_, err := f.guard.Execute(f.ctx, coordinatorAdapterTool{name: name, readOnly: true}, map[string]any{"command": "find . -exec touch changed {} +"}, func(context.Context) (*pubtool.Result, error) { calls++; return &pubtool.Result{Output: "done"}, nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			if calls != 2 || len(f.latest(t).Operations) != 2 {
				t.Fatalf("opaque code bypassed operation guard: calls=%d", calls)
			}
		})
	}
}

func TestCoordinatorOperationCrashHelper(t *testing.T) {
	if os.Getenv("METIS_OPERATION_ADAPTER_CRASH_HELPER") != "1" {
		return
	}
	store := projectcoord.NewStore(os.Getenv("METIS_OPERATION_STATE"))
	var item projectcoord.WorkItem
	if err := json.Unmarshal([]byte(os.Getenv("METIS_OPERATION_ITEM")), &item); err != nil {
		t.Fatal(err)
	}
	workspace, runID := os.Getenv("METIS_OPERATION_WORKSPACE"), os.Getenv("METIS_OPERATION_RUN")
	ctx, cancel := coordinatorOperationContext(context.Background(), store, workspace, runID, item, item.Owner)
	defer cancel(nil)
	_, _ = pubtool.OperationExecutorFromContext(ctx).Execute(ctx, coordinatorAdapterTool{name: "remote_write"}, map[string]any{"request": "one"}, func(context.Context) (*pubtool.Result, error) {
		if err := os.WriteFile(os.Getenv("METIS_OPERATION_EXTERNAL_COUNT"), []byte("1"), 0600); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "external-write-applied")
		time.Sleep(time.Hour)
		return &pubtool.Result{Output: "receipt"}, nil
	})
}

func TestCoordinatorOperationProcessKillDoesNotRepeatAppliedWriteWithoutReceipt(t *testing.T) {
	f := newCoordinatorAdapterFixture(t)
	item, err := f.store.Renew(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	itemBytes, _ := json.Marshal(item)
	countFile := filepath.Join(t.TempDir(), "remote-counter")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestCoordinatorOperationCrashHelper$")
	command.Env = append(os.Environ(), "METIS_OPERATION_ADAPTER_CRASH_HELPER=1", "METIS_OPERATION_STATE="+f.store.Root(), "METIS_OPERATION_WORKSPACE="+f.run.Workspace, "METIS_OPERATION_RUN="+f.run.ID, "METIS_OPERATION_ITEM="+string(itemBytes), "METIS_OPERATION_EXTERNAL_COUNT="+countFile)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "external-write-applied\n" {
			t.Fatalf("helper did not perform write: %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not reach the crash window")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	for time.Now().Before(item.Lease.ExpiresAt.Add(20 * time.Millisecond)) {
		time.Sleep(10 * time.Millisecond)
	}
	run := f.latest(t)
	if len(run.Operations) != 1 || run.Operations[0].Status != projectcoord.OperationUncertain {
		t.Fatalf("crash window did not hold the applied write: %+v", run.Operations)
	}
	if _, err := f.store.ClaimNext(f.run.Workspace, f.run.ID, projectcoord.ClaimOptions{Worker: "replacement"}); !errors.Is(err, projectcoord.ErrNoReadyWork) {
		t.Fatalf("replacement automatically retried missing receipt: %v", err)
	}
	count, err := os.ReadFile(countFile)
	if err != nil || string(count) != "1" {
		t.Fatalf("external write repeated: %q, %v", count, err)
	}
}
