package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Capture the contexts actually reaching the child loop's provider, including
// the hard lifecycle used by InterruptBlock tools, and keep the child running.
type deadlineCaptureProvider struct {
	hangingProvider
	started chan context.Context
}

func (p *deadlineCaptureProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	p.started <- ctx
	return &hangingStream{ctx: ctx}, nil
}

func TestAgentToolParentAndChildDeadlines(t *testing.T) {
	for _, background := range []bool{false, true} {
		for _, tc := range []struct {
			name        string
			parentLimit time.Duration
			childLimit  int
			wantLimit   time.Duration
		}{
			{name: "parent_sooner", parentLimit: 2 * time.Second, childLimit: 3600, wantLimit: 2 * time.Second},
			{name: "child_sooner", parentLimit: time.Hour, childLimit: 1, wantLimit: time.Second},
			{name: "parent_only", parentLimit: 2 * time.Second, wantLimit: 2 * time.Second},
			{name: "child_only", childLimit: 1, wantLimit: time.Second},
		} {
			t.Run(fmt.Sprintf("background_%t/%s", background, tc.name), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					parentCtx := context.Background()
					if tc.parentLimit > 0 {
						var cancelParent context.CancelFunc
						parentCtx, cancelParent = context.WithTimeout(parentCtx, tc.parentLimit)
						defer cancelParent()
					}
					provider := &deadlineCaptureProvider{started: make(chan context.Context, 1)}
					roster := agent.NewRoster(0)
					defer func() {
						roster.CancelAll()
						synctest.Wait()
					}()
					tool := NewAgent(permission.New(permission.ModeBypass), provider, tools.NewRegistry(), "model", "system").WithRoster(roster)
					finished := make(chan *tools.Result, 1)
					startedAt := time.Now()
					go func() {
						result, err := tool.Execute(parentCtx, map[string]any{
							"prompt":            "keep working until the deadline",
							"run_in_background": background,
							"timeout_seconds":   tc.childLimit,
						})
						if err != nil {
							t.Errorf("Execute: %v", err)
						}
						finished <- result
					}()
					synctest.Wait()
					var childCtx context.Context
					select {
					case childCtx = <-provider.started:
					default:
						t.Fatal("child did not reach the provider")
					}
					lifecycleCtx := agent.ToolLifecycleContextFromContext(childCtx)
					if lifecycleCtx == nil {
						t.Fatal("child has no hard lifecycle context")
					}
					for name, ctx := range map[string]context.Context{"child": childCtx, "hard_lifecycle": lifecycleCtx} {
						deadline, ok := ctx.Deadline()
						if !ok || !deadline.Equal(startedAt.Add(tc.wantLimit)) {
							t.Fatalf("%s deadline = (%v, %t), want %v", name, deadline, ok, startedAt.Add(tc.wantLimit))
						}
					}
					time.Sleep(tc.wantLimit - time.Nanosecond)
					synctest.Wait()
					if childCtx.Err() != nil || lifecycleCtx.Err() != nil {
						t.Fatal("child was canceled before the earliest deadline")
					}
					time.Sleep(time.Nanosecond)
					synctest.Wait()
					if childCtx.Err() == nil || lifecycleCtx.Err() == nil {
						t.Fatal("earliest deadline did not cancel both child and hard lifecycle")
					}
					if got := roster.Count(); got != 0 {
						t.Fatalf("roster count after deadline = %d, want 0", got)
					}
					select {
					case result := <-finished:
						if result == nil || result.IsError == background {
							t.Fatalf("Execute result = %+v, background=%t", result, background)
						}
						if !background && !strings.Contains(result.Output, "timed out after "+tc.wantLimit.String()+".") {
							t.Errorf("timeout result = %q, want effective budget %s", result.Output, tc.wantLimit)
						}
					default:
						t.Fatal("Execute did not finish after the deadline")
					}
				})
			})
		}
	}
}

func TestBackgroundAgentIgnoresParentCancelButPreservesDeadline(t *testing.T) {
	for _, parentLimit := range []time.Duration{0, 2 * time.Second} {
		t.Run(parentLimit.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parentCtx, cancelParent := context.WithCancel(context.Background())
				defer cancelParent()
				if parentLimit > 0 {
					var cancelDeadline context.CancelFunc
					parentCtx, cancelDeadline = context.WithTimeout(parentCtx, parentLimit)
					defer cancelDeadline()
				}
				provider := &deadlineCaptureProvider{started: make(chan context.Context, 1)}
				roster := agent.NewRoster(0)
				defer func() {
					roster.CancelAll()
					synctest.Wait()
				}()
				tool := NewAgent(permission.New(permission.ModeBypass), provider, tools.NewRegistry(), "model", "system").WithRoster(roster)
				result, err := tool.Execute(parentCtx, map[string]any{
					"prompt":            "work past the parent turn",
					"run_in_background": true,
				})
				if err != nil || result == nil || result.IsError {
					t.Fatalf("background spawn = (%+v, %v)", result, err)
				}
				childCtx := <-provider.started
				lifecycleCtx := agent.ToolLifecycleContextFromContext(childCtx)
				if lifecycleCtx == nil {
					t.Fatal("child has no hard lifecycle context")
				}
				cancelParent()
				synctest.Wait()
				if childCtx.Err() != nil || lifecycleCtx.Err() != nil {
					t.Fatal("ordinary parent cancel stopped the background child")
				}
				time.Sleep(3 * time.Second)
				synctest.Wait()
				for name, ctx := range map[string]context.Context{"child": childCtx, "hard_lifecycle": lifecycleCtx} {
					if parentLimit == 0 {
						if _, ok := ctx.Deadline(); ok || ctx.Err() != nil {
							t.Errorf("unlimited %s acquired a deadline or cancellation: %v", name, ctx.Err())
						}
					} else if ctx.Err() == nil {
						t.Errorf("%s lost the parent's deadline after parent cancel", name)
					}
				}
			})
		})
	}
}
