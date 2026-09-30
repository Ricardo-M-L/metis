package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/tasks"
)

// The legacy environment names are retained for Desktop launcher compatibility.
// SLOTS now denotes the shared root + child execution budget, not a reserved
// child partition. Ordinary CLI runs leave SLOT_DIR unset.
const (
	desktopSubagentSlotDirEnv         = "METIS_DESKTOP_SUBAGENT_SLOT_DIR"
	desktopSubagentSlotsEnv           = "METIS_DESKTOP_SUBAGENT_SLOTS"
	desktopSubagentCapEnv             = "METIS_DESKTOP_SUBAGENT_CAP"
	desktopSchedulerInspectionTimeout = 2 * time.Second
)

var ErrDesktopSubagentCapacity = errors.New("Desktop execution capacity is occupied")

// DesktopExecutionConfig lets in-process Desktop turns use the same scheduler
// as isolated workers without changing process-wide environment variables.
// Owner identifies one top-level turn; child contexts retain it even when a
// background sub-agent detaches from turn cancellation.
type DesktopExecutionConfig struct {
	SlotDir          string
	TotalAgentSlots  int
	SubagentsPerRoot int
	Owner            string
}

type desktopExecutionConfigKey struct{}

// WithDesktopExecutionConfig binds one in-process Desktop turn to its shared
// execution pool. It does not alter CLI or isolated-worker environments.
func WithDesktopExecutionConfig(ctx context.Context, config DesktopExecutionConfig) context.Context {
	return context.WithValue(ctx, desktopExecutionConfigKey{}, config)
}

// DesktopExecutionConfigFromContext returns the explicit in-process binding,
// if one exists. A present but invalid binding is rejected at acquisition.
func DesktopExecutionConfigFromContext(ctx context.Context) (DesktopExecutionConfig, bool) {
	if ctx == nil {
		return DesktopExecutionConfig{}, false
	}
	config, ok := ctx.Value(desktopExecutionConfigKey{}).(DesktopExecutionConfig)
	return config, ok
}

func desktopExecutionLease(ctx context.Context, child bool) (*DesktopExecutionLease, error) {
	config, inProcess := DesktopExecutionConfigFromContext(ctx)
	var dir string
	var total, perRoot int
	if inProcess {
		dir = strings.TrimSpace(config.SlotDir)
		total, perRoot = config.TotalAgentSlots, config.SubagentsPerRoot
		if dir == "" || total < 1 || total > 64 || perRoot < 1 || perRoot > 64 {
			return nil, errors.New("desktop scheduler: invalid in-process execution config")
		}
	} else {
		dir = strings.TrimSpace(os.Getenv(desktopSubagentSlotDirEnv))
		if dir == "" {
			return nil, nil
		}
		var err error
		total, err = strconv.Atoi(strings.TrimSpace(os.Getenv(desktopSubagentSlotsEnv)))
		if err != nil || total < 1 || total > 64 {
			return nil, errors.New("desktop scheduler: invalid total capacity")
		}
		if raw := strings.TrimSpace(os.Getenv(desktopSubagentCapEnv)); raw != "" {
			perRoot, err = strconv.Atoi(raw)
			if err != nil || perRoot < 1 || perRoot > 64 {
				return nil, errors.New("desktop scheduler: invalid per-root capacity")
			}
		}
	}
	owner := fmt.Sprintf("worker-%d", os.Getpid())
	if inProcess && strings.TrimSpace(config.Owner) != "" {
		owner = strings.TrimSpace(config.Owner)
	}
	if parent := desktopLeaseFromContext(ctx); child && parent != nil {
		owner = parent.owner
	} else if !child && !inProcess {
		// Continuations of one Desktop turn retain the same root group even
		// while children from its previous model round are still running.
		if sessionID := tasks.SessionIDFromContext(ctx); sessionID != "" {
			owner += ":" + sessionID
		}
	}
	lease := &DesktopExecutionLease{owner: owner, acquire: func(ctx context.Context) (func(), error) {
		return acquireDesktopExecution(ctx, dir, total, perRoot, child, owner)
	}}
	if err := lease.resume(ctx); err != nil {
		return nil, err
	}
	return lease, nil
}

// AcquireDesktopSubagentSlot is also used by focused scheduler tests. Agents
// use AcquireDesktopTeammateExecution so their queued identity is visible first.
func AcquireDesktopSubagentSlot(ctx context.Context) (func(), error) {
	lease, err := desktopExecutionLease(ctx, true)
	if err != nil {
		return nil, err
	}
	return lease.Close, nil
}
func RegisterDesktopTeammate(ctx context.Context, roster *Roster, t *Teammate, _ bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, configured := DesktopExecutionConfigFromContext(ctx); !configured && strings.TrimSpace(os.Getenv(desktopSubagentSlotDirEnv)) == "" {
		return roster.Register(t)
	}
	return roster.RegisterQueued(t)
}

// AcquireDesktopTeammateExecution always replaces an inherited parent's lease.
// No provider execution begins until this queued identity owns its own permit.
func AcquireDesktopTeammateExecution(ctx context.Context, roster *Roster, t *Teammate) (context.Context, error) {
	lease, err := desktopExecutionLease(ctx, true)
	if err != nil {
		return ctx, err
	}
	if lease == nil {
		return ctx, nil
	}
	if err = ctx.Err(); err == nil {
		err = roster.tryStartQueued(t, lease.Close, lease)
	}
	if err != nil {
		lease.Close()
		return ctx, err
	}
	return context.WithValue(ctx, desktopLeaseKey{}, lease), nil
}
func enterDesktopLoop(ctx context.Context) (context.Context, func(), error) {
	if desktopLeaseFromContext(ctx) != nil {
		return ctx, func() {}, nil
	}
	lease, err := desktopExecutionLease(ctx, false)
	if err != nil {
		return ctx, func() {}, err
	}
	if lease == nil {
		return ctx, func() {}, nil
	}
	return context.WithValue(ctx, desktopLeaseKey{}, lease), lease.Close, nil
}
func waitDesktopCapacity(ctx context.Context) error {
	timer := time.NewTimer(25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
