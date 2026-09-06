package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestLoopTurnWallClockUnlimitedWaitsForSixHourJob(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		unset bool
	}{
		{name: "unset", unset: true},
		{name: "empty"},
		{name: "zero", value: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("METIS_TURN_MAX_SECONDS", tc.value)
			if tc.unset {
				if err := os.Unsetenv("METIS_TURN_MAX_SECONDS"); err != nil {
					t.Fatal(err)
				}
			}
			synctest.Test(t, func(t *testing.T) {
				notify := make(chan jobs.Notification, 1)
				provider := &queuedStreamProvider{streams: []llm.StreamReader{
					toolUseStream("six-hour-wait", "AwaitedJob", `{}`),
					textStream("waiting for the background job"),
					textStream("SIX_HOUR_JOB_FINISHED"),
				}}
				registry := tools.NewRegistry()
				registry.Register(awaitedNotificationTool{})
				loop := NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 6)
				loop.JobNotify = notify
				loop.AppendUser("finish after the awaited background job completes")

				// The bubble advances virtual time only: this covers a full six-hour
				// wait and the following iteration without a real wall-clock delay.
				ctx, cancel := context.WithTimeout(context.Background(), 7*time.Hour)
				defer cancel()
				go func() {
					select {
					case <-time.After(6 * time.Hour):
						notify <- jobs.Notification{
							JobID: "bg_wait_test", Status: jobs.StatusCompleted,
							Command: "six-hour awaited job", Elapsed: 6 * time.Hour,
						}
					case <-ctx.Done():
					}
				}()
				out := make(chan Event, 64)
				if err := loop.Run(ctx, out); err != nil {
					t.Fatalf("Run: %v", err)
				}
				close(out)
				var reasons []string
				var turnEnds int
				for event := range out {
					if event.Kind == EventLoopDone {
						reasons = append(reasons, event.StopReason)
					}
					if event.Kind == EventTurnEnd {
						turnEnds++
					}
				}
				if len(reasons) != 1 || reasons[0] != "end_turn" {
					t.Fatalf("LoopDone reasons = %v, want one end_turn after six hours", reasons)
				}
				if turnEnds != 2 {
					t.Fatalf("TurnEnd count = %d, want tool batch and waiting-response boundaries", turnEnds)
				}
				requests := provider.capturedRequests()
				if len(requests) != 3 || !requestContains(requests[2], "bg_wait_test") {
					t.Fatalf("provider requests = %d, want continuation with completed job", len(requests))
				}
				if !historyContainsText(loop.History(), "SIX_HOUR_JOB_FINISHED") {
					t.Fatal("history lost final continuation after the six-hour wait")
				}
			})
		})
	}
}

func TestLoopTurnWallClockAwaitedDeadlineAtExactBoundary(t *testing.T) {
	t.Setenv("METIS_TURN_MAX_SECONDS", "1")
	synctest.Test(t, func(t *testing.T) {
		provider := &queuedStreamProvider{streams: []llm.StreamReader{
			toolUseStream("exact-boundary-wait", "AwaitedJob", `{}`),
			textStream("waiting for a job that never completes"),
		}}
		registry := tools.NewRegistry()
		registry.Register(awaitedNotificationTool{})
		loop := NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 6)
		loop.JobNotify = make(chan jobs.Notification)
		loop.AppendUser("wait for the awaited job")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		started := time.Now()
		out := make(chan Event, 64)
		if err := loop.Run(ctx, out); err != nil {
			t.Fatalf("Run: %v, want a classified turn_wall_clock stop", err)
		}
		close(out)
		if elapsed := time.Since(started); elapsed != time.Second {
			t.Fatalf("Run elapsed = %s, want exactly one virtual second", elapsed)
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("turn budget canceled the parent context: %v", err)
		}
		if got := len(provider.capturedRequests()); got != 2 {
			t.Fatalf("provider calls = %d, want no request after the awaited deadline", got)
		}
		var loopDone int
		for event := range out {
			if event.Kind == EventLoopDone {
				loopDone++
				if event.StopReason != "turn_wall_clock" {
					t.Fatalf("stop reason = %s, want turn_wall_clock", event.StopReason)
				}
			}
		}
		if loopDone != 1 {
			t.Fatalf("LoopDone count = %d, want one", loopDone)
		}
	})
}

func TestLoopTurnWallClockAwaitedWaitPreservesParentCancellation(t *testing.T) {
	for _, budget := range []string{"", "0", "1"} {
		for _, terminal := range []string{"cancel", "deadline"} {
			t.Run(budget+"/"+terminal, func(t *testing.T) {
				t.Setenv("METIS_TURN_MAX_SECONDS", budget)
				synctest.Test(t, func(t *testing.T) {
					notify := make(chan jobs.Notification, 4)
					provider := &queuedStreamProvider{streams: []llm.StreamReader{
						toolUseStream("parent-cancel-wait", "AwaitedJob", `{}`),
						&notifyingStream{
							inner: textStream("waiting for the awaited job"), notify: notify,
							notification: jobs.Notification{
								JobID: "unrelated-before-parent-cancel", Status: jobs.StatusCompleted,
							},
						},
					}}
					registry := tools.NewRegistry()
					registry.Register(awaitedNotificationTool{})
					hooks := NewHookRegistry()
					var sessionEnds int
					var sessionStop string
					hooks.Register(SessionEndHandler(func(_ context.Context, _ HookContext, _ int, reason string) {
						sessionEnds++
						sessionStop = reason
					}))
					loop := NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), hooks, "system", 6)
					loop.JobNotify = notify
					loop.AppendUser("wait for the awaited job")
					var ctx context.Context
					var cancel context.CancelFunc
					wantErr := context.Canceled
					if terminal == "deadline" {
						ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
						wantErr = context.DeadlineExceeded
					} else {
						ctx, cancel = context.WithCancel(context.Background())
						time.AfterFunc(100*time.Millisecond, cancel)
					}
					defer cancel()
					out := make(chan Event, 64)
					if err := loop.Run(ctx, out); !errors.Is(err, wantErr) {
						t.Fatalf("Run error = %v, want %v", err, wantErr)
					}
					close(out)
					if sessionEnds != 1 || sessionStop != "error" {
						t.Fatalf("SessionEnd = (%d, %q), want (1, error)", sessionEnds, sessionStop)
					}
					if got := len(provider.capturedRequests()); got != 2 {
						t.Fatalf("provider calls = %d, want no continuation after parent cancellation", got)
					}
					var injectionEvents int
					for event := range out {
						if event.Kind == EventLoopDone {
							t.Fatalf("parent cancellation emitted LoopDone(%s)", event.StopReason)
						}
						if event.Kind == EventInfo && strings.Contains(event.Info, "notification(s) injected") {
							injectionEvents++
						}
					}
					if injectionEvents != 1 || !historyContainsText(loop.History(), "unrelated-before-parent-cancel") {
						t.Fatal("parent cancellation lost a collected notification or its injection event")
					}
				})
			})
		}
	}
}

type wallClockInFlightTool struct{ lowOutputTool }

func (wallClockInFlightTool) Execute(ctx context.Context, _ map[string]any) (*tools.Result, error) {
	time.Sleep(2 * time.Second)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &tools.Result{Output: "IN_FLIGHT_TOOL_FINISHED"}, nil
}

func TestLoopTurnWallClockBudgetStopsAtIterationBoundary(t *testing.T) {
	t.Setenv("METIS_TURN_MAX_SECONDS", "1")
	synctest.Test(t, func(t *testing.T) {
		provider := &queuedStreamProvider{streams: []llm.StreamReader{
			toolUseStream("in-flight-tool", "LowOutput", `{}`),
		}}
		registry := tools.NewRegistry()
		registry.Register(wallClockInFlightTool{})
		loop := NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 6)
		loop.AppendUser("run the tool")
		out := make(chan Event, 32)
		if err := loop.Run(context.Background(), out); err != nil {
			t.Fatalf("Run: %v", err)
		}
		close(out)
		var reasons []string
		for event := range out {
			if event.Kind == EventLoopDone {
				reasons = append(reasons, event.StopReason)
			}
		}
		if len(reasons) != 1 || reasons[0] != "turn_wall_clock" {
			t.Fatalf("LoopDone reasons = %v, want one turn_wall_clock", reasons)
		}
		if got := len(provider.capturedRequests()); got != 1 {
			t.Fatalf("provider calls = %d, want stop before the next request", got)
		}
		var toolFinished bool
		for _, message := range loop.History() {
			for _, block := range message.Content {
				if block.Type == "tool_result" && block.ToolUseID == "in-flight-tool" && block.ToolResult == "IN_FLIGHT_TOOL_FINISHED" && !block.IsError {
					toolFinished = true
				}
			}
		}
		if !toolFinished {
			t.Fatal("explicit turn budget interrupted the in-flight tool")
		}
	})
}

func TestLoopTurnWallClockRejectsInvalidConfiguration(t *testing.T) {
	for _, value := range []string{"-1", "invalid", "1.5", "9223372036854775807", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("METIS_TURN_MAX_SECONDS", value)
			provider := &queuedStreamProvider{}
			hooks := NewHookRegistry()
			var sessionEnds int
			var sessionStop string
			hooks.Register(SessionEndHandler(func(_ context.Context, _ HookContext, _ int, reason string) {
				sessionEnds++
				sessionStop = reason
			}))
			loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypassPermissions), hooks, "system", 6)
			// Configuration failures must still run the normal history repair and
			// session cleanup used by every other Run error path.
			loop.Messages = []llm.Message{{
				Role: llm.RoleAssistant,
				Content: []llm.ContentBlock{{
					Type: "tool_use", ToolUseID: "orphan-before-invalid-budget", ToolName: "Read",
				}},
			}}
			out := make(chan Event, 32)
			err := loop.Run(context.Background(), out)
			close(out)
			if err == nil || !strings.Contains(err.Error(), "METIS_TURN_MAX_SECONDS") {
				t.Fatalf("Run error = %v, want a named configuration error", err)
			}
			if len(provider.capturedRequests()) != 0 {
				t.Fatal("invalid turn budget started a provider request")
			}
			if sessionEnds != 1 || sessionStop != "error" {
				t.Fatalf("SessionEnd = (%d, %q), want (1, error)", sessionEnds, sessionStop)
			}
			var errorEvents int
			for event := range out {
				if event.Kind == EventError && errors.Is(event.Err, err) {
					errorEvents++
				}
				if event.Kind == EventLoopDone {
					t.Fatalf("invalid configuration emitted LoopDone(%s)", event.StopReason)
				}
			}
			if errorEvents != 1 {
				t.Fatalf("matching EventError count = %d, want one", errorEvents)
			}
			var repaired bool
			for _, message := range loop.History() {
				for _, block := range message.Content {
					if block.Type == "tool_result" && block.ToolUseID == "orphan-before-invalid-budget" {
						repaired = true
					}
				}
			}
			if !repaired {
				t.Fatal("configuration error skipped deferred orphan repair")
			}
		})
	}
}
