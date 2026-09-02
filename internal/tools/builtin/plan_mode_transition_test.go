package builtin_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

type blockingPlanController struct {
	mu      sync.Mutex
	prePlan string
	plan    bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingPlanController) PrePlanMode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prePlan
}

func (c *blockingPlanController) SetPrePlanMode(mode string) {
	c.mu.Lock()
	c.prePlan = mode
	shouldBlock := mode == string(permission.ModeFullAccess)
	entered, release := c.entered, c.release
	c.mu.Unlock()
	if shouldBlock {
		c.once.Do(func() { close(entered) })
		<-release
	}
}

func (c *blockingPlanController) SetPlanMode(plan bool) {
	c.mu.Lock()
	c.plan = plan
	c.mu.Unlock()
}

func TestEnterPlanModeSerializesLineageWithPermissionStateCapture(t *testing.T) {
	gate := permission.New(permission.ModeFullAccess)
	releaseLease, allowed, reason := gate.TryAcquireToolDispatchLease()
	if !allowed {
		t.Fatalf("acquire isolated plan batch lease: %s", reason)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(releaseLease) }
	t.Cleanup(release)
	controller := &blockingPlanController{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	ctx := agent.WithPlanController(context.Background(), controller)
	tool := builtin.NewEnterPlanModeWithGate(gate)

	executeDone := make(chan error, 1)
	go func() {
		_, err := tool.Execute(ctx, map[string]any{})
		executeDone <- err
	}()
	select {
	case <-controller.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("EnterPlanMode did not reach its lineage write")
	}

	type captureResult struct {
		state rtpkg.PermissionModeState
		err   error
	}
	captured := make(chan captureResult, 1)
	go func() {
		state, err := rtpkg.CapturePermissionModeState(gate, controller)
		captured <- captureResult{state: state, err: err}
	}()
	select {
	case got := <-captured:
		t.Fatalf("capture crossed an incomplete plan transition: state=%+v err=%v", got.state, got.err)
	case <-time.After(50 * time.Millisecond):
	}

	close(controller.release)
	select {
	case err := <-executeDone:
		if err != nil {
			t.Fatalf("EnterPlanMode: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EnterPlanMode did not finish")
	}
	release()
	select {
	case got := <-captured:
		if got.err != nil {
			t.Fatalf("capture after plan transition: %v", got.err)
		}
		if got.state.Mode != permission.ModePlan || got.state.PrePlanMode != string(permission.ModeFullAccess) {
			t.Fatalf("captured state = %+v, want plan with fullAccess lineage", got.state)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("capture did not finish after plan transition")
	}
}
