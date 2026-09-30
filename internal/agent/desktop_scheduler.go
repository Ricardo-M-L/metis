package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Requests have kernel-held liveness locks. A crashed worker therefore leaves
// neither an occupied execution permit nor a queue head blocking other roots.
type desktopRequest struct {
	Sequence   uint64 `json:"sequence"`
	Owner      string `json:"owner"`
	Child      bool   `json:"child"`
	ChildLimit int    `json:"childLimit"`
}
type desktopSchedulerState struct {
	Sequence  uint64 `json:"sequence"`
	LastOwner string `json:"lastOwner"`
}

func desktopSchedulerLock(ctx context.Context, dir string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		release, ok, err := tryAcquireDesktopSlot(filepath.Join(dir, "scheduler.lock"))
		if err != nil {
			return nil, err
		}
		if ok {
			return release, nil
		}
		if err := waitDesktopCapacity(ctx); err != nil {
			return nil, err
		}
	}
}
func readDesktopJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("desktop scheduler metadata: %w", err)
	}
	return nil
}
func writeDesktopJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if filepath.Base(path) == "scheduler.json" {
		temp, err := os.CreateTemp(filepath.Dir(path), ".scheduler-state-")
		if err != nil {
			return err
		}
		defer os.Remove(temp.Name())
		if _, err = temp.Write(data); err != nil {
			temp.Close()
			return err
		}
		if err = temp.Close(); err != nil {
			return err
		}
		return os.Rename(temp.Name(), path)
	}
	return os.WriteFile(path, data, 0600)
}
func desktopSchedulerStateAt(dir string) (desktopSchedulerState, error) {
	var state desktopSchedulerState
	err := readDesktopJSON(filepath.Join(dir, "scheduler.json"), &state)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return state, err
}

// scanDesktopSlots must hold scheduler.lock. Free files can contain old metadata;
// only a held kernel lock denotes an active execution.
func scanDesktopSlots(dir string, limit int) ([]desktopRequest, int, error) {
	active := []desktopRequest{}
	free := -1
	for i := 0; i < limit; i++ {
		path := filepath.Join(dir, fmt.Sprintf("slot-%02d.lock", i))
		release, ok, err := tryAcquireDesktopSlot(path)
		if err != nil {
			return nil, -1, err
		}
		if ok {
			release()
			if free < 0 {
				free = i
			}
			continue
		}
		var request desktopRequest
		if err := readDesktopJSON(path, &request); err != nil {
			return nil, -1, err
		}
		active = append(active, request)
	}
	return active, free, nil
}
func scanDesktopQueue(dir string, own uint64) ([]desktopRequest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	queue := []desktopRequest{}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "ticket-") || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var request desktopRequest
		// Check liveness before decoding: a crash can leave an incomplete ticket.
		if entry.Name() != fmt.Sprintf("ticket-%020d.lock", own) {
			release, ok, err := tryAcquireDesktopSlot(path)
			if err != nil {
				return nil, err
			}
			if ok {
				release()
				_ = os.Remove(path)
				continue
			}
		}
		if err := readDesktopJSON(path, &request); err != nil {
			return nil, err
		}
		queue = append(queue, request)
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i].Sequence < queue[j].Sequence })
	return queue, nil
}
func desktopNextRequest(queue, active []desktopRequest, lastOwner string) uint64 {
	children := map[string]int{}
	for _, request := range active {
		if request.Child {
			children[request.Owner]++
		}
	}
	// Pick the oldest eligible request in each root, then rotate through all
	// roots. Alternating merely "not last owner" can starve a third root behind
	// two roots with large old backlogs.
	heads := map[string]uint64{}
	for _, request := range queue {
		if request.Child && request.ChildLimit > 0 && children[request.Owner] >= request.ChildLimit {
			continue
		}
		if _, exists := heads[request.Owner]; !exists {
			heads[request.Owner] = request.Sequence
		}
	}
	owners := make([]string, 0, len(heads))
	for owner := range heads {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	for _, owner := range owners {
		if owner > lastOwner {
			return heads[owner]
		}
	}
	if len(owners) > 0 {
		return heads[owners[0]]
	}
	return 0
}

func acquireDesktopExecution(ctx context.Context, dir string, total, perRoot int, child bool, owner string) (func(), error) {
	if total < 1 || total > 64 || perRoot < 0 || perRoot > 64 {
		return nil, errors.New("desktop scheduler: invalid execution capacity")
	}
	unlock, err := desktopSchedulerLock(ctx, dir)
	if err != nil {
		return nil, err
	}
	state, err := desktopSchedulerStateAt(dir)
	if err != nil {
		unlock()
		return nil, err
	}
	state.Sequence++
	request := desktopRequest{Sequence: state.Sequence, Owner: owner, Child: child, ChildLimit: perRoot}
	ticket := filepath.Join(dir, fmt.Sprintf("ticket-%020d.lock", request.Sequence))
	releaseTicket, ok, err := tryAcquireDesktopSlot(ticket)
	if err == nil && !ok {
		err = errors.New("desktop scheduler: duplicate queue ticket")
	}
	if err == nil {
		err = writeDesktopJSON(ticket, request)
	}
	if err == nil {
		err = writeDesktopJSON(filepath.Join(dir, "scheduler.json"), state)
	}
	unlock()
	if err != nil {
		if releaseTicket != nil {
			releaseTicket()
		}
		return nil, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), desktopSchedulerInspectionTimeout)
		defer cancel()
		unlock, err := desktopSchedulerLock(cleanupCtx, dir)
		if err == nil {
			releaseTicket()
			_ = os.Remove(ticket)
			unlock()
		} else {
			releaseTicket()
		}

	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unlock, err := desktopSchedulerLock(ctx, dir)
		if err != nil {
			return nil, err
		}
		release, err := tryAdmitDesktopExecution(ctx, dir, total, request)
		unlock()
		if err != nil {
			return nil, err
		}
		if release != nil {
			return release, nil
		}
		if err := waitDesktopCapacity(ctx); err != nil {
			return nil, err
		}
	}
}
func tryAdmitDesktopExecution(ctx context.Context, dir string, total int, request desktopRequest) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	active, free, err := scanDesktopSlots(dir, total)
	if err != nil || free < 0 {
		return nil, err
	}
	queue, err := scanDesktopQueue(dir, request.Sequence)
	if err != nil {
		return nil, err
	}
	state, err := desktopSchedulerStateAt(dir)
	if err != nil {
		return nil, err
	}
	if desktopNextRequest(queue, active, state.LastOwner) != request.Sequence {
		return nil, nil
	}
	path := filepath.Join(dir, fmt.Sprintf("slot-%02d.lock", free))
	release, ok, err := tryAcquireDesktopSlot(path)
	if err != nil || !ok {
		return nil, err
	}
	if err = writeDesktopJSON(path, request); err != nil {
		release()
		return nil, err
	}
	state.LastOwner = request.Owner
	if err = writeDesktopJSON(filepath.Join(dir, "scheduler.json"), state); err != nil {
		release()
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// DesktopSchedulerIdle includes queued children after their root turn ends.
// Callers must also serialize new root launches while applying new limits.
func DesktopSchedulerIdle(dir string) (bool, error) {
	if strings.TrimSpace(dir) == "" {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), desktopSchedulerInspectionTimeout)
	defer cancel()
	unlock, err := desktopSchedulerLock(ctx, dir)
	if err != nil {
		return false, err
	}
	defer unlock()
	active, _, err := scanDesktopSlots(dir, 64)
	if err != nil {
		return false, err
	}
	queue, err := scanDesktopQueue(dir, 0)
	return len(active) == 0 && len(queue) == 0, err
}

// DesktopSchedulerOccupancy samples the kernel-held execution permits under
// scheduler.lock. Unlike roster lifecycle states, this measures slots that are
// actually occupied across all workers sharing dir at this instant.
func DesktopSchedulerOccupancy(dir string, total int) (int, error) {
	if strings.TrimSpace(dir) == "" {
		return 0, nil
	}
	if total < 1 || total > 64 {
		return 0, errors.New("desktop scheduler: invalid total capacity")
	}
	// Status is polled frequently; contention should skip one sample rather
	// than hold an HTTP request for the longer idle-inspection timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	unlock, err := desktopSchedulerLock(ctx, dir)
	if err != nil {
		return 0, err
	}
	defer unlock()
	active, _, err := scanDesktopSlots(dir, total)
	return len(active), err
}

type desktopExecutionPhase uint32

const (
	desktopExecutionWaitingSlot desktopExecutionPhase = iota
	desktopExecutionExecuting
	desktopExecutionWaitingBackground
	desktopExecutionWaitingChildren
	desktopExecutionFinished
)

func (phase desktopExecutionPhase) String() string {
	switch phase {
	case desktopExecutionExecuting:
		return "executing"
	case desktopExecutionWaitingBackground:
		return "waiting_background"
	case desktopExecutionWaitingChildren:
		return "waiting_children"
	case desktopExecutionFinished:
		return "finished"
	default:
		return "waiting_slot"
	}
}

// DesktopExecutionLease belongs to one loop, never its descendants. Pausing a
// coordinating parent releases both its global and per-root execution charge;
// resuming joins the fair queue again. Logical agent identities remain intact.
type DesktopExecutionLease struct {
	mu      sync.Mutex
	owner   string
	acquire func(context.Context) (func(), error)
	release func()
	closed  bool
	// Read without mu so a status sampler can observe waiting_slot while a
	// resume call is blocked in the scheduler queue holding mu.
	phase atomic.Uint32
}

func (l *DesktopExecutionLease) executionSnapshot() (string, bool) {
	if l == nil {
		return "executing", false // ordinary CLI: no Desktop execution slot
	}
	phase := desktopExecutionPhase(l.phase.Load())
	return phase.String(), phase == desktopExecutionExecuting
}

func (l *DesktopExecutionLease) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	// Publish a conservative held bit before freeing the permit. A status
	// sampler must never count this lease and its replacement as both held.
	l.phase.Store(uint32(desktopExecutionFinished))
	if l.release != nil {
		l.release()
		l.release = nil
	}
}
func (l *DesktopExecutionLease) pause(reason desktopExecutionPhase) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		// Clear the published held bit before another worker can acquire
		// the released slot. The kernel lock remains the exact global count.
		l.phase.Store(uint32(reason))
	}
	if l.release != nil {
		l.release()
		l.release = nil
	}
}
func (l *DesktopExecutionLease) resume(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return context.Canceled
	}
	if l.release != nil {
		return ctx.Err()
	}
	l.phase.Store(uint32(desktopExecutionWaitingSlot))
	release, err := l.acquire(ctx)
	if err == nil {
		l.release = release
		l.phase.Store(uint32(desktopExecutionExecuting))
	}
	return err
}

type desktopLeaseKey struct{}

func desktopLeaseFromContext(ctx context.Context) *DesktopExecutionLease {
	lease, _ := ctx.Value(desktopLeaseKey{}).(*DesktopExecutionLease)
	return lease
}

// YieldDesktopExecution is used only at explicit coordination/wait boundaries.
// Ordinary file/command tool execution keeps its loop's execution charge.
func YieldDesktopExecution(ctx context.Context) func(context.Context) error {
	return yieldDesktopExecution(ctx, desktopExecutionWaitingChildren)
}

func yieldDesktopExecution(ctx context.Context, reason desktopExecutionPhase) func(context.Context) error {
	lease := desktopLeaseFromContext(ctx)
	lease.pause(reason)
	return lease.resume
}
