package webui

import (
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestComposerPermissionFallbackSerializesListenerAndStateCapture(t *testing.T) {
	gate := permission.New(permission.ModeFullAccess)
	loop := agent.NewLoop(nil, tools.NewRegistry(), gate, nil, "system", 1)
	server := &Server{loop: loop}
	listenerEntered := make(chan struct{})
	releaseListener := make(chan struct{})
	gate.SetModeChangeListener(func(mode permission.Mode) {
		if mode == permission.ModePlan {
			close(listenerEntered)
			<-releaseListener
		}
		loop.SetPlanMode(mode == permission.ModePlan)
	})

	applyDone := make(chan error, 1)
	go func() { applyDone <- server.applyPermissionMode(permission.ModePlan) }()
	select {
	case <-listenerEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("composer permission listener did not start")
	}

	type captureResult struct {
		state rtpkg.PermissionModeState
		err   error
	}
	captured := make(chan captureResult, 1)
	go func() {
		state, err := rtpkg.CapturePermissionModeState(gate, loop)
		captured <- captureResult{state: state, err: err}
	}()
	select {
	case got := <-captured:
		t.Fatalf("capture bypassed composer listener drain: state=%+v err=%v", got.state, got.err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseListener)
	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatalf("applyPermissionMode: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("composer permission transition did not finish")
	}
	select {
	case got := <-captured:
		if got.err != nil {
			t.Fatalf("capture after composer transition: %v", got.err)
		}
		if got.state.Mode != permission.ModePlan || got.state.PrePlanMode != string(permission.ModeFullAccess) {
			t.Fatalf("captured state = %+v, want plan with fullAccess lineage", got.state)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("capture did not finish after composer transition")
	}
}
