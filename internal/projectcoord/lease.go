package projectcoord

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type CompletionInput struct {
	Worker string
	Token  string
	Output string
}

func (i *WorkItem) assertClaim(worker, token string) error {
	if err := i.assertOwner(worker); err != nil {
		return err
	}
	if token == "" || i.Lease == nil || i.Lease.Token != token {
		return fmt.Errorf("work item %q claim token does not match the active lease", i.ID)
	}
	return nil
}

// Renew extends only the caller's active claim; expired claims cannot be revived.
func (s *Store) Renew(workspace, runID, itemID, worker, token string, duration time.Duration) (WorkItem, error) {
	if duration < time.Second || duration > 24*time.Hour {
		return WorkItem{}, errors.New("lease must be between 1s and 24h")
	}
	var renewed WorkItem
	_, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		item, err := run.item(itemID)
		if err != nil {
			return false, err
		}
		if err := item.assertClaim(worker, token); err != nil {
			return false, err
		}
		now := s.now()
		item.Lease.ExpiresAt = now.Add(duration)
		item.UpdatedAt, run.UpdatedAt = now, now
		renewed = cloneItem(*item)
		return true, nil
	})
	return renewed, err
}

// KeepLease cancels work if ownership is lost; stop joins the renewal goroutine.
func (s *Store) KeepLease(ctx context.Context, workspace, runID string, item WorkItem, duration time.Duration) (context.Context, func() error, error) {
	if item.Lease == nil {
		return nil, nil, errors.New("worker has no active lease")
	}
	if _, err := s.Renew(workspace, runID, item.ID, item.Owner, item.Lease.Token, duration); err != nil {
		return nil, nil, err
	}
	workCtx, cancelWork := context.WithCancelCause(ctx)
	heartbeatCtx, stopHeartbeat := context.WithCancel(workCtx)
	done := make(chan struct{})
	var renewalErr error
	go func() {
		defer close(done)
		ticker := time.NewTicker(min(duration/3, 30*time.Second))
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if _, err := s.Renew(workspace, runID, item.ID, item.Owner, item.Lease.Token, duration); err != nil {
					renewalErr = fmt.Errorf("project worker lease lost: %w", err)
					cancelWork(renewalErr)
					return
				}
			}
		}
	}()
	var once sync.Once
	return workCtx, func() error {
		once.Do(func() { stopHeartbeat(); <-done; cancelWork(context.Canceled) })
		return renewalErr
	}, nil
}
