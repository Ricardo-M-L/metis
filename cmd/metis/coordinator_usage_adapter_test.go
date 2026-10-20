package main

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

func TestCoordinatorUsageAdapterPersistsFramesWithoutLegacyDoubleCharge(t *testing.T) {
	workspace := t.TempDir()
	store := projectcoord.NewStore(t.TempDir())
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "Actual durable frame boundary", Items: []projectcoord.WorkItemInput{{ID: "work", Subject: "Work", Prompt: "work"}}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(workspace, run.ID, projectcoord.ClaimOptions{Worker: "worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	tracker := budget.NewTracker(0, budget.Rates{InputPerMTok: 1, OutputPerMTok: 2})
	tracker.SetPersistence(func(value float64) error {
		return store.RecordSpend(workspace, run.ID, item.ID, item.Owner, item.Lease.Token, value)
	})
	owner := &coordinatorUsageObserver{store: store, workspace: workspace, runID: run.ID, itemID: item.ID, worker: item.Owner, token: item.Lease.Token, provider: "selected", tracker: tracker}
	id, err := projectcoord.NewUsageRequestID()
	if err != nil {
		t.Fatal(err)
	}
	r, err := owner.Begin(context.Background(), transport.RequestMetadata{ClientRequestID: id, Provider: "anthropic", EndpointSHA256: strings.Repeat("a", 64), Model: "selected-model", RequestSHA256: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Header("provider-request-1"); err != nil {
		t.Fatal(err)
	}
	snapshot := transport.UsageSnapshot{ResponseID: "message-1", InputTokens: 10000, OutputTokens: 2000, UsageReported: true}
	if err := r.Snapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	// Open a different Store before closing the reader: SIGKILL cannot undo
	// this already committed usage/response identity.
	disk, err := projectcoord.NewStore(store.Root()).Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(disk.UsageRequests) != 1 || disk.UsageRequests[0].ResponseID != "message-1" || !disk.UsageRequests[0].UsageReported || disk.UsageRequests[0].Usage.Output != 2000 {
		t.Fatalf("stream frame not durably visible: %+v", disk.UsageRequests)
	}
	if err := r.Snapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := tracker.AddUsage(10000, 2000, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.End(transport.UsageInterrupted); err != nil {
		t.Fatal(err)
	}
	disk, err = store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(disk.SpentUSD-.014) > 1e-12 || math.Abs(tracker.SpentUSD()-.014) > 1e-12 {
		t.Fatalf("double billed stream: store=%v tracker=%v", disk.SpentUSD, tracker.SpentUSD())
	}
	if len(projectcoord.UnresolvedUsageRequests(disk)) != 1 {
		t.Fatal("interrupted request was treated as final billing")
	}
}

func TestCoordinatorUsageAdapterKeepsMissingUsageUnknownAndBoundsTokens(t *testing.T) {
	workspace := t.TempDir()
	store := projectcoord.NewStore(t.TempDir())
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "Unknown usage", Items: []projectcoord.WorkItemInput{{ID: "work", Subject: "work", Prompt: "work"}}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(workspace, run.ID, projectcoord.ClaimOptions{Worker: "worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	owner := &coordinatorUsageObserver{store: store, workspace: workspace, runID: run.ID, itemID: item.ID, worker: item.Owner, token: item.Lease.Token, provider: "selected", tracker: budget.NewTracker(0, budget.Rates{})}
	id, _ := projectcoord.NewUsageRequestID()
	observer, err := owner.Begin(context.Background(), transport.RequestMetadata{ClientRequestID: id, EndpointSHA256: strings.Repeat("a", 64), Model: "model", RequestSHA256: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.Snapshot(transport.UsageSnapshot{ResponseID: "unreported"}); err != nil {
		t.Fatal(err)
	}
	if err := observer.Snapshot(transport.UsageSnapshot{InputTokens: -1, UsageReported: true}); err == nil {
		t.Fatal("negative usage accepted")
	}
	if err := observer.End(transport.UsageCompleted); err != nil {
		t.Fatal(err)
	}
	disk, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projectcoord.UnresolvedUsageRequests(disk)) != 1 || disk.UsageRequests[0].UsageReported {
		t.Fatal("missing usage became authoritative zero")
	}
}

func TestCoordinatorUsageAdapterUsesLatestAbsoluteCorrection(t *testing.T) {
	workspace := t.TempDir()
	store := projectcoord.NewStore(t.TempDir())
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "Correct preliminary usage", Items: []projectcoord.WorkItemInput{{ID: "work", Subject: "work", Prompt: "work"}}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(workspace, run.ID, projectcoord.ClaimOptions{Worker: "worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	tracker := budget.NewTracker(.001, budget.Rates{InputPerMTok: 2, OutputPerMTok: 4, CacheReadPerMTok: .5, CacheWritePerMTok: .1})
	owner := &coordinatorUsageObserver{store: store, workspace: workspace, runID: run.ID, itemID: item.ID, worker: item.Owner, token: item.Lease.Token, provider: "selected", tracker: tracker}
	id, _ := projectcoord.NewUsageRequestID()
	observer, err := owner.Begin(context.Background(), transport.RequestMetadata{ClientRequestID: id, EndpointSHA256: strings.Repeat("a", 64), Model: "model", RequestSHA256: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.Snapshot(transport.UsageSnapshot{InputTokens: 556, UsageReported: true}); err != nil {
		t.Fatal(err)
	}
	if !tracker.Exceeded() {
		t.Fatal("initial reported estimate missing from live budget")
	}
	final := transport.UsageSnapshot{InputTokens: 179, OutputTokens: 74, CacheCreationInputTokens: 512, UsageReported: true}
	if err := observer.Snapshot(final); err != nil {
		t.Fatal(err)
	}
	if tracker.Exceeded() || math.Abs(tracker.SpentUSD()-.0007052) > 1e-12 {
		t.Fatalf("cache split retained old uncached count: %v", tracker.SpentUSD())
	}
	if err := observer.Snapshot(transport.UsageSnapshot{UsageReported: true}); err != nil {
		t.Fatal(err)
	}
	if tracker.Exceeded() || tracker.SpentUSD() != 0 {
		t.Fatalf("zero provider correction kept old budget: %v", tracker.SpentUSD())
	}
	if err := observer.End(transport.UsageCompleted); err != nil {
		t.Fatal(err)
	}
	if err := observer.Snapshot(final); err == nil {
		t.Fatal("a late snapshot rewrote completed usage")
	}
	disk, err := projectcoord.NewStore(store.Root()).Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if disk.SpentUSD != 0 || disk.UsageRequests[0].Sequence != 4 || disk.UsageRequests[0].AccountingVersion != 2 {
		t.Fatal("correction and observation order were not durably preserved")
	}
}

func TestCoordinatorUsageAdapterPreservesPhysicalModelPriceOverride(t *testing.T) {
	workspace := t.TempDir()
	store := projectcoord.NewStore(t.TempDir())
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "Physical model prices", Items: []projectcoord.WorkItemInput{{ID: "work", Subject: "work", Prompt: "work"}}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(workspace, run.ID, projectcoord.ClaimOptions{Worker: "worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	tracker := budget.NewTrackerLazy(.1, func() (budget.Rates, bool) {
		t.Fatal("physical price override consulted unrelated tracker model")
		return budget.Rates{}, true
	})
	prices := projectcoord.UsagePriceSnapshot{InputPerMillionUSD: 1.5, Source: "physical-model-catalogue"}
	owner := &coordinatorUsageObserver{store: store, workspace: workspace, runID: run.ID, itemID: item.ID, worker: item.Owner, token: item.Lease.Token, provider: "selected", tracker: tracker, requestPriceOverride: &prices}
	id, _ := projectcoord.NewUsageRequestID()
	observer, err := owner.Begin(context.Background(), transport.RequestMetadata{ClientRequestID: id, EndpointSHA256: strings.Repeat("a", 64), Model: "different-physical-model", RequestSHA256: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	prices.InputPerMillionUSD = 999
	if err := observer.Snapshot(transport.UsageSnapshot{InputTokens: 100_000, UsageReported: true}); err != nil {
		t.Fatal(err)
	}
	disk, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if owner.tracker != tracker || disk.UsageRequests[0].Prices.InputPerMillionUSD != 1.5 || math.Abs(tracker.SpentUSD()-.15) > 1e-12 || !tracker.Exceeded() {
		t.Fatal("override replaced the shared tracker or changed reserved physical model pricing")
	}
}
