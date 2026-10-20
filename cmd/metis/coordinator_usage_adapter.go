package main

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

type coordinatorUsageObserver struct {
	syncMu                                            sync.Mutex
	dispatchOnce                                      sync.Once
	dispatchGate                                      chan struct{}
	identityMu                                        sync.RWMutex
	identityOnce                                      sync.Once
	identityReady                                     chan struct{}
	identityWait                                      time.Duration
	requestPrices                                     func(string) projectcoord.UsagePriceSnapshot
	requireKnownPrices                                bool
	store                                             *projectcoord.Store
	workspace, runID, itemID, worker, token, provider string
	tracker                                           *budget.Tracker
	requestPriceOverride                              *projectcoord.UsagePriceSnapshot
	itemBaseline, runBaseline                         float64
}

// The execution-wide observer exists before runtime helpers are constructed.
// Its identity barrier prevents a detached startup helper from reserving a
// request under a guessed provider name while setup resolves the trusted route.
func coordinatorExecutionUsageContext(ctx context.Context, store *projectcoord.Store, workspace, runID string, run projectcoord.Run, item projectcoord.WorkItem, worker string, tracker *budget.Tracker) (context.Context, *coordinatorUsageObserver) {
	if tracker == nil {
		tracker = budget.NewTracker(projectcoord.RemainingBudget(run, item), budget.Rates{})
	}
	o := &coordinatorUsageObserver{store: store, workspace: workspace, runID: runID, itemID: item.ID, worker: worker, token: item.Lease.Token, tracker: tracker, itemBaseline: item.SpentUSD, runBaseline: run.SpentUSD, identityReady: make(chan struct{}), requireKnownPrices: run.MaxBudgetUSD > 0 || item.MaxBudgetUSD > 0}
	o.requestPrices = func(model string) projectcoord.UsagePriceSnapshot {
		rates := rtpkg.NewBudgetTracker(0, model).PricingSnapshot()
		source := "models.dev catalogue estimate for physical request model"
		if rates.InputPerMTok <= 0 && rates.OutputPerMTok <= 0 {
			source = "physical request model pricing unavailable"
		}
		return projectcoord.UsagePriceSnapshot{InputPerMillionUSD: rates.InputPerMTok, OutputPerMillionUSD: rates.OutputPerMTok, CacheReadPerMillionUSD: rates.CacheReadPerMTok, CacheWritePerMillionUSD: rates.CacheWritePerMTok, Source: source}
	}
	return transport.WithUsageObserver(ctx, o), o
}

func (o *coordinatorUsageObserver) bindExecutionIdentity(provider, model string) {
	o.identityOnce.Do(func() {
		o.identityMu.Lock()
		o.provider = provider
		o.identityMu.Unlock()
		o.tracker.SetRatesResolver(func() (budget.Rates, bool) { return rtpkg.NewBudgetTracker(0, model).PricingSnapshot(), true })
		close(o.identityReady)
	})
}

func coordinatorUsageContext(ctx context.Context, store *projectcoord.Store, workspace, runID string, run projectcoord.Run, item projectcoord.WorkItem, worker, provider string, tracker *budget.Tracker) context.Context {
	return transport.WithUsageObserver(ctx, &coordinatorUsageObserver{store: store, workspace: workspace, runID: runID, itemID: item.ID, worker: worker, token: item.Lease.Token, provider: provider, tracker: tracker, itemBaseline: item.SpentUSD, runBaseline: run.SpentUSD})
}

func (o *coordinatorUsageObserver) syncBudget() error {
	o.syncMu.Lock()
	defer o.syncMu.Unlock()
	run, err := o.store.Get(o.workspace, o.runID)
	if err != nil {
		return err
	}
	for _, item := range run.Items {
		if item.ID == o.itemID {
			o.tracker.SetObservedSpend(math.Max(0, math.Max(item.SpentUSD-o.itemBaseline, run.SpentUSD-o.runBaseline)))
			return nil
		}
	}
	return errors.New("usage work item is unavailable")
}

func (o *coordinatorUsageObserver) Begin(ctx context.Context, meta transport.RequestMetadata) (transport.RequestUsageObserver, error) {
	if o.identityReady != nil {
		wait := o.identityWait
		if wait <= 0 {
			wait = 5 * time.Second
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-o.identityReady:
		case <-timer.C:
			return nil, errors.New("coordinator execution identity did not become ready before dispatch; no provider request was made")
		}
	}
	o.dispatchOnce.Do(func() { o.dispatchGate = make(chan struct{}, 1); o.dispatchGate <- struct{}{} })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-o.dispatchGate:
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { o.dispatchGate <- struct{}{} }) }
	reserved := false
	defer func() {
		if !reserved {
			release()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := o.syncBudget(); err != nil {
		return nil, err
	}
	if o.tracker.Exceeded() {
		return nil, errors.New("durable task budget is exhausted")
	}
	var prices projectcoord.UsagePriceSnapshot
	if o.requestPrices != nil {
		prices = o.requestPrices(meta.Model)
	} else if o.requestPriceOverride != nil {
		prices = *o.requestPriceOverride
	} else {
		rates := o.tracker.PricingSnapshot()
		prices = projectcoord.UsagePriceSnapshot{InputPerMillionUSD: rates.InputPerMTok, OutputPerMillionUSD: rates.OutputPerMTok, CacheReadPerMillionUSD: rates.CacheReadPerMTok, CacheWritePerMillionUSD: rates.CacheWritePerMTok, Source: "models.dev catalogue estimate"}
	}
	if o.requireKnownPrices && prices.InputPerMillionUSD <= 0 && prices.OutputPerMillionUSD <= 0 {
		return nil, errors.New("durable coordinator USD budget requires known physical model pricing")
	}
	o.identityMu.RLock()
	provider := o.provider
	o.identityMu.RUnlock()
	if provider == "" {
		provider = meta.Provider
	}
	record, err := o.store.BeginUsageRequest(o.workspace, o.runID, projectcoord.BeginUsageRequestInput{ClientRequestID: meta.ClientRequestID, ItemID: o.itemID, Worker: o.worker, ClaimToken: o.token, Provider: provider, EndpointSHA256: meta.EndpointSHA256, Model: meta.Model, RequestSHA256: meta.RequestSHA256, Prices: prices})
	if err != nil {
		return nil, err
	}
	reserved = true
	return &coordinatorRequestUsage{owner: o, record: record, releaseDispatch: release}, nil
}

type coordinatorRequestUsage struct {
	mu              sync.Mutex
	owner           *coordinatorUsageObserver
	record          projectcoord.UsageRequestRecord
	releaseDispatch func()
}

func (r *coordinatorRequestUsage) persist(input projectcoord.UsageSnapshotInput) error {
	x, err := r.owner.store.RecordUsageSnapshot(r.owner.workspace, r.owner.runID, input)
	if err != nil {
		return err
	}
	r.record = x
	return r.owner.syncBudget()
}

func (r *coordinatorRequestUsage) input() projectcoord.UsageSnapshotInput {
	return projectcoord.UsageSnapshotInput{ClientRequestID: r.record.ClientRequestID, ItemID: r.record.ItemID, Worker: r.record.Worker, ClaimToken: r.record.ClaimToken, ProviderRequestID: r.record.ProviderRequestID, ResponseID: r.record.ResponseID, ResponseStatusCode: r.record.ResponseStatusCode, Usage: r.record.Usage, UsageReported: r.record.UsageReported, Status: r.record.Status, Sequence: r.record.Sequence + 1}
}

func (r *coordinatorRequestUsage) HTTPStatus(status int) error {
	if status < 100 || status > 599 {
		return errors.New("invalid HTTP response status")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	in := r.input()
	in.ResponseStatusCode = status
	return r.persist(in)
}

func (r *coordinatorRequestUsage) Header(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	in := r.input()
	in.ProviderRequestID = id
	return r.persist(in)
}

func (r *coordinatorRequestUsage) Snapshot(s transport.UsageSnapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	values := []int64{s.InputTokens, s.OutputTokens, s.CacheReadInputTokens, s.CacheCreationInputTokens}
	for _, value := range values {
		if value < 0 || uint64(value) > uint64(^uint(0)>>1) {
			return errors.New("provider token count exceeds native ledger range")
		}
	}
	in := r.input()
	if s.ResponseID != "" {
		in.ResponseID = s.ResponseID
	}
	in.UsageReported = in.UsageReported || s.UsageReported
	if s.UsageReported {
		in.Usage = projectcoord.TokenUsage{Input: int(s.InputTokens), Output: int(s.OutputTokens), CacheRead: int(s.CacheReadInputTokens), CacheWrite: int(s.CacheCreationInputTokens)}
	}
	return r.persist(in)
}

func (r *coordinatorRequestUsage) End(status transport.UsageStatus) error {
	if r.releaseDispatch != nil {
		defer r.releaseDispatch()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	in := r.input()
	switch status {
	case transport.UsageCompleted:
		in.Status = projectcoord.UsageRequestCompleted
	case transport.UsageInterrupted:
		in.Status = projectcoord.UsageRequestInterrupted
	case transport.UsageRejected:
		in.Status = projectcoord.UsageRequestRejected
	default:
		return errors.New("unsupported durable usage outcome")
	}
	return r.persist(in)
}
