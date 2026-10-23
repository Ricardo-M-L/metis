package main

import (
	"errors"
	"fmt"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// The listener calls commit with the mode/lineage already settled by the
// transition. It must not reacquire Gate.RunModeTransition from a tool's shared
// admission lease. The sampler only reads the last successfully fsynced value.
type desktopWorkerPermission struct {
	mu    sync.Mutex
	store *session.Store
	id    string
	last  *desktopipc.PermissionState
	err   error
}

func (p *desktopWorkerPermission) commit(mode permission.Mode, prePlan string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	next := desktopipc.PermissionState{SessionID: p.id, Mode: string(mode), PrePlanMode: prePlan}
	if !next.Valid() {
		p.err = errors.New("desktop worker permission state is incomplete")
	} else if p.last == nil || *p.last != next {
		p.err = p.store.CheckpointPermissionState(p.id, next.Mode, next.PrePlanMode)
		if p.err == nil {
			p.last = &next
		}
	}
	return p.err
}

func (p *desktopWorkerPermission) snapshot() *desktopipc.PermissionState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil || p.err != nil {
		return nil
	}
	copy := *p.last
	return &copy
}

func (p *desktopWorkerPermission) checkpoint(rt *runtime) error {
	// Save executes at producer boundaries, after tools have joined. Runtime's
	// mode listener has already crossed the durable boundary synchronously.
	return p.commit(rt.gate.Mode(), rt.loop.PrePlanMode())
}

func bindDesktopWorkerPermission(rt *runtime, worker *desktopWorkerBridge) (*desktopWorkerPermission, error) {
	if rt == nil || rt.gate == nil || rt.loop == nil || rt.store == nil || rt.sessionID == "" {
		return nil, errors.New("desktop worker requires a durable permission state")
	}
	p := &desktopWorkerPermission{store: rt.store, id: rt.sessionID}
	if err := p.checkpoint(rt); err != nil {
		return nil, fmt.Errorf("persist initial desktop worker permission state: %w", err)
	}
	rt.permissionCheckpointMu.Lock()
	rt.permissionCheckpoint = func(mode permission.Mode, prePlan string) error { return p.commit(mode, prePlan) }
	rt.permissionCheckpointFailure = func() { worker.fail(errors.New("desktop worker permission checkpoint failed")) }
	rt.permissionCheckpointMu.Unlock()
	return p, nil
}

func (rt *runtime) checkpointPermissionTransition(mode permission.Mode) {
	rt.permissionCheckpointMu.RLock()
	commit, failed := rt.permissionCheckpoint, rt.permissionCheckpointFailure
	rt.permissionCheckpointMu.RUnlock()
	if commit == nil || rt.loop == nil || rt.gate.Mode() != mode {
		return
	}
	if err := commit(mode, rt.loop.PrePlanMode()); err != nil {
		// dontAsk still admits allow rules; Plan is the required read-only
		// posture after a failed permission commit. Cancel first so descendants
		// and the next provider request cannot continue under a failed approval.
		if failed != nil {
			failed()
		}
		rt.loop.SetPrePlanMode(string(permission.ModeDefault))
		rt.loop.SetPlanMode(true)
		if rt.gate.Mode() != permission.ModePlan {
			rt.gate.SetMode(permission.ModePlan)
		}
	}
}
