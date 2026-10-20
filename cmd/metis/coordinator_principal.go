package main

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

func checkPersistentJobReady(binding *principal.Binding) error {
	if binding == nil {
		return nil
	}
	j, err := persistentJobStore().Get(binding.JobID)
	if err != nil {
		return err
	}
	if err = principal.ValidateBinding(binding, j.Definition.Workspace); err != nil {
		return err
	}
	if j.ConfigSHA256 != binding.ConfigSHA256 {
		return errors.New("persistent job definition mismatch")
	}
	if j.Status != principal.Active {
		return principal.ErrInactive
	}
	if j.UnresolvedRequests > 0 {
		return principal.ErrUsageUnknown
	}
	b := j.Definition.Budget
	if b.MaxUSD > 0 && j.SpentUSD >= b.MaxUSD || b.MaxInputTokens > 0 && j.InputTokens >= b.MaxInputTokens || b.MaxOutputTokens > 0 && j.OutputTokens >= b.MaxOutputTokens {
		return principal.ErrBudget
	}
	return nil
}

func syncPersistentJobUsage(store *projectcoord.Store, binding principal.Binding) error {
	summaries, err := persistentJobStore().RefreshRuns(binding.JobID)
	if err != nil {
		return err
	}
	for _, summary := range summaries {
		run, err := store.Get(binding.Definition.Workspace, summary.RunID)
		if err != nil {
			return err
		}
		projection, err := persistentJobRunUsage(run, binding)
		if err != nil {
			return err
		}
		if _, err = persistentJobStore().RecordRunUsage(binding, projection); err != nil {
			return err
		}
	}
	return nil
}

func persistentJobRunUsage(run projectcoord.Run, binding principal.Binding) (principal.RunUsage, error) {
	if run.Workspace != binding.Definition.Workspace {
		return principal.RunUsage{}, errors.New("persistent job accounting workspace mismatch")
	}
	projection := principal.RunUsage{RunID: run.ID, SourceRevision: run.Revision, Status: string(run.Status)}
	items := make(map[string]bool)
	for _, item := range run.Items {
		if item.PrincipalBinding != nil && item.PrincipalBinding.JobID == binding.JobID {
			if item.PrincipalBinding.ConfigSHA256 != binding.ConfigSHA256 {
				return projection, errors.New("persistent job accounting definition mismatch")
			}
			items[item.ID] = true
			projection.SpentUSD += item.SpentUSD
		}
	}
	if len(items) == 0 {
		return projection, errors.New("persistent job run no longer contains its assigned work")
	}
	for _, record := range run.UsageRequests {
		if !items[record.ItemID] {
			continue
		}
		for _, pair := range []struct {
			total *int64
			value int
		}{{&projection.InputTokens, record.Usage.Input}, {&projection.OutputTokens, record.Usage.Output}, {&projection.CacheReadTokens, record.Usage.CacheRead}, {&projection.CacheWriteTokens, record.Usage.CacheWrite}} {
			if pair.value < 0 || *pair.total > math.MaxInt64-int64(pair.value) {
				return projection, errors.New("persistent job physical token accounting overflow")
			}
			*pair.total += int64(pair.value)
		}
	}
	for _, record := range projectcoord.UnresolvedUsageRequests(run) {
		if items[record.ItemID] {
			projection.UnresolvedRequests++
		}
	}
	return projection, nil
}

func preparePersistentJobWorker(ctx context.Context, store *projectcoord.Store, run projectcoord.Run, item projectcoord.WorkItem, worker string, ttl time.Duration) (context.Context, principal.Job, principal.Lease, func(), error) {
	if item.PrincipalBinding == nil {
		return ctx, principal.Job{}, principal.Lease{}, func() {}, nil
	}
	binding := *item.PrincipalBinding
	if err := validatePersistentJobRoute(binding); err != nil {
		return ctx, principal.Job{}, principal.Lease{}, func() {}, err
	}
	// Recover the narrow crash window between persisting a bound assignment
	// and registering its run. Registration is idempotent and validates the
	// immutable definition before any provider reservation or dispatch.
	if _, err := persistentJobStore().RegisterRun(binding, run.ID); err != nil {
		return ctx, principal.Job{}, principal.Lease{}, func() {}, err
	}
	if err := syncPersistentJobUsage(store, binding); err != nil {
		return ctx, principal.Job{}, principal.Lease{}, func() {}, err
	}
	executionID, err := projectcoord.NewUsageRequestID()
	if err != nil {
		return ctx, principal.Job{}, principal.Lease{}, func() {}, err
	}
	lease, err := persistentJobStore().ClaimExecution(binding, executionID, worker, ttl)
	if err != nil {
		return ctx, principal.Job{}, principal.Lease{}, func() {}, err
	}
	j, err := persistentJobStore().Get(binding.JobID)
	if err != nil {
		_ = persistentJobStore().ReleaseExecution(binding, lease)
		return ctx, principal.Job{}, principal.Lease{}, func() {}, err
	}
	workCtx, stop := persistentJobStore().KeepExecution(ctx, binding, lease, ttl)
	return workCtx, j, lease, stop, nil
}

func persistentJobRuntimeFlags(item projectcoord.WorkItem, j principal.Job) (*cliFlags, error) {
	if item.PrincipalBinding == nil {
		return &cliFlags{agentProfile: item.AgentProfile}, nil
	}
	if err := principal.ValidateBinding(item.PrincipalBinding, j.Definition.Workspace); err != nil {
		return nil, err
	}
	f := &cliFlags{agentProfile: item.AgentProfile, agentProfileSnapshot: rtpkg.AgentProfileFromSnapshot(item.PrincipalBinding.Definition.Profile), provider: item.PrincipalBinding.Definition.Provider, providerSet: true, jobID: j.ID, jobSessionDir: j.SessionDir, jobMemoryDir: j.MemoryDir}
	// session.NewSession writes <id>.jsonl. A restarted worker resumes exactly
	// that transcript, including acknowledged handoff envelopes.
	if info, err := os.Lstat(filepath.Join(j.SessionDir, j.SessionID+".jsonl")); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("persistent job transcript must be a regular file")
		}
		f.resumeID = j.SessionID
	} else if errors.Is(err, os.ErrNotExist) {
		f.newSessionID = j.SessionID
	} else {
		return nil, err
	}
	return f, nil
}

func persistentJobRemainingBudget(run projectcoord.Run, item projectcoord.WorkItem, j principal.Job) float64 {
	remaining := projectcoord.RemainingBudget(run, item)
	if item.PrincipalBinding != nil && j.Definition.Budget.MaxUSD > 0 {
		jobRemaining := j.RemainingUSD()
		if remaining == 0 || jobRemaining < remaining {
			remaining = jobRemaining
		}
	}
	return remaining
}

// Job requests are serialized, including background sub-agent requests. A
// second provider dispatch waits for the first physical request to settle,
// then reads all registered run ledgers before deciding whether it may start.
type persistentJobUsageObserver struct {
	delegate *coordinatorUsageObserver
	store    *projectcoord.Store
	binding  principal.Binding
	lease    principal.Lease
	gate     chan struct{}
}

func coordinatorPersistentJobUsageContext(ctx context.Context, store *projectcoord.Store, workspace, runID string, run projectcoord.Run, item projectcoord.WorkItem, worker, provider string, tracker *budget.Tracker, lease principal.Lease) context.Context {
	if item.PrincipalBinding == nil {
		return coordinatorUsageContext(ctx, store, workspace, runID, run, item, worker, provider, tracker)
	}
	o := &persistentJobUsageObserver{delegate: &coordinatorUsageObserver{store: store, workspace: workspace, runID: runID, itemID: item.ID, worker: worker, token: item.Lease.Token, provider: provider, tracker: tracker, itemBaseline: item.SpentUSD, runBaseline: run.SpentUSD}, store: store, binding: *item.PrincipalBinding, lease: lease, gate: make(chan struct{}, 1)}
	o.gate <- struct{}{}
	return transport.WithUsageObserver(ctx, o)
}

func (o *persistentJobUsageObserver) Begin(ctx context.Context, meta transport.RequestMetadata) (transport.RequestUsageObserver, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-o.gate:
	}
	release := func() { o.gate <- struct{}{} }
	if err := syncPersistentJobUsage(o.store, o.binding); err != nil {
		release()
		return nil, err
	}
	if _, err := persistentJobStore().CheckExecution(o.binding, o.lease); err != nil {
		release()
		return nil, err
	}
	rates := rtpkg.NewBudgetTracker(0, meta.Model).PricingSnapshot()
	o.delegate.requestPriceOverride = &projectcoord.UsagePriceSnapshot{InputPerMillionUSD: rates.InputPerMTok, OutputPerMillionUSD: rates.OutputPerMTok, CacheReadPerMillionUSD: rates.CacheReadPerMTok, CacheWritePerMillionUSD: rates.CacheWritePerMTok, Source: "models.dev catalogue estimate for physical request model"}
	if o.binding.Definition.Budget.MaxUSD > 0 {
		// Current catalogue APIs omit whether an all-zero price is explicitly
		// free or simply absent. Treat that ambiguity as unavailable pricing.
		if rates.InputPerMTok <= 0 && rates.OutputPerMTok <= 0 {
			release()
			return nil, errors.New("persistent job USD budget requires known model pricing; choose a priced model or a lifetime token budget")
		}
	}
	delegate, err := o.delegate.Begin(ctx, meta)
	if err != nil {
		release()
		return nil, err
	}
	if err = syncPersistentJobUsage(o.store, o.binding); err != nil {
		// The delegate has reserved and acquired its live dispatch gate. No
		// network request may be made after this failure, but its uncertain
		// reservation remains recorded and both live gates must be released.
		_ = delegate.End(transport.UsageInterrupted)
		release()
		return nil, err
	}
	return &persistentJobRequestUsage{owner: o, delegate: delegate, release: release}, nil
}

type persistentJobRequestUsage struct {
	owner    *persistentJobUsageObserver
	delegate transport.RequestUsageObserver
	release  func()
	once     sync.Once
}

func (r *persistentJobRequestUsage) HTTPStatus(status int) error {
	if observer, ok := r.delegate.(transport.UsageHTTPStatusObserver); ok {
		if err := observer.HTTPStatus(status); err != nil {
			return err
		}
		return syncPersistentJobUsage(r.owner.store, r.owner.binding)
	}
	return nil
}

func (r *persistentJobRequestUsage) Header(id string) error {
	if err := r.delegate.Header(id); err != nil {
		return err
	}
	return syncPersistentJobUsage(r.owner.store, r.owner.binding)
}
func (r *persistentJobRequestUsage) Snapshot(s transport.UsageSnapshot) error {
	if err := r.delegate.Snapshot(s); err != nil {
		return err
	}
	return syncPersistentJobUsage(r.owner.store, r.owner.binding)
}
func (r *persistentJobRequestUsage) End(s transport.UsageStatus) error {
	defer r.once.Do(r.release)
	err := r.delegate.End(s)
	return errors.Join(err, syncPersistentJobUsage(r.owner.store, r.owner.binding))
}
