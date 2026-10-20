package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

func ordinaryUsageFixture(t *testing.T) (*projectcoord.Store, projectcoord.Run, projectcoord.WorkItem) {
	t.Helper()
	store := projectcoord.NewStore(t.TempDir())
	run, err := store.Create(projectcoord.CreateInput{Workspace: t.TempDir(), Goal: "ordinary coordinator unknown-usage hold", Items: []projectcoord.WorkItemInput{{ID: "work", Subject: "work", Prompt: "work", MaxAttempts: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "original-worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return store, run, item
}

func ordinaryUsagePost(t *testing.T, ctx context.Context, client *http.Client, url, prompt string) (*http.Response, error) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": "fixture-model", "stream": false, "messages": []map[string]string{{"role": "user", "content": prompt}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer private-test-credential")
	return client.Do(req)
}

func TestOrdinaryCoordinatorTimeoutHoldsUsageUntilReceiptAndExplicitRecovery(t *testing.T) {
	store, run, item := ordinaryUsageFixture(t)
	var requests atomic.Int32
	receipts := make(chan projectcoord.UsageReceipt, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n == 1 {
			// The controlled upstream separately emits its authoritative usage
			// receipt even though the client's response-header wait times out.
			evidence := sha256.Sum256([]byte(`{"input_tokens":19,"output_tokens":4}`))
			endpoint := sha256.Sum256([]byte("http://" + r.Host + r.URL.EscapedPath()))
			receipts <- projectcoord.UsageReceipt{ReceiptID: "fixture-authoritative-receipt", EvidenceSHA256: hex.EncodeToString(evidence[:]), Authority: "controlled-http-test-upstream", Provider: "fixture-provider", EndpointSHA256: hex.EncodeToString(endpoint[:]), Model: "fixture-model", ClientRequestID: r.Header.Get(transport.HeaderRequestID), Usage: projectcoord.TokenUsage{Input: 19, Output: 4}}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Second):
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fixture-response","model":"fixture-model","usage":{"prompt_tokens":17,"completion_tokens":3},"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()
	tracker := budget.NewTracker(0, budget.Rates{InputPerMTok: 1, OutputPerMTok: 2})
	ctx := coordinatorUsageContext(context.Background(), store, run.Workspace, run.ID, run, item, item.Owner, "fixture-provider", tracker)
	client := transport.NewHTTPClient(75*time.Millisecond, "openai")
	endpoint := server.URL + "/v1/chat/completions"
	_, err := ordinaryUsagePost(t, ctx, client, endpoint, "first request")
	if err == nil {
		t.Fatal("slow response unexpectedly completed")
	}
	_, retryErr := ordinaryUsagePost(t, ctx, client, endpoint, "different retry arguments cannot bypass unknown consumption")
	var boundary *transport.UsageObserverError
	if !errors.As(retryErr, &boundary) || boundary.Code != "provider_usage_unresolved" || !errors.Is(boundary.Err, projectcoord.ErrUsageUnresolved) || requests.Load() != 1 {
		t.Fatalf("unknown usage allowed physical retry: posts=%d error=%v", requests.Load(), retryErr)
	}
	failure := coordinatorWorkerFailure(context.Background(), transport.SafeUsageObserverClassification(retryErr))
	if failure.Code != "provider_usage_unresolved" || failure.Recoverable || failure.AutoRecovered || strings.Contains(failure.Summary, "private-") {
		t.Fatalf("safe usage hold classification lost: %+v", failure)
	}
	if _, err = recordCoordinatorFailure(store, run.Workspace, run.ID, item, item.Owner, failure, ""); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Status != projectcoord.RunBlocked || blocked.Items[0].Status != projectcoord.ItemFailed || blocked.Items[0].Failure.AutoRecovered {
		t.Fatal("unsettled usage became automatic recovery")
	}
	if _, err = store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "replacement-worker", Lease: time.Minute}); !errors.Is(err, projectcoord.ErrNoReadyWork) {
		t.Fatalf("service reclaimed unsettled work: %v", err)
	}
	// This is a controlled upstream receipt in the real import schema, not a
	// zero fabricated from the client's timeout or elapsed wall-clock time.
	var receipt projectcoord.UsageReceipt
	select {
	case receipt = <-receipts:
	case <-time.After(time.Second):
		t.Fatal("controlled upstream receipt was not emitted")
	}
	if _, err = store.ImportUsageReceipt(run.Workspace, run.ID, receipt); err != nil {
		t.Fatal(err)
	}
	settled, err := store.Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projectcoord.UnresolvedUsageRequests(settled)) != 0 || settled.Items[0].Status != projectcoord.ItemFailed {
		t.Fatal("receipt implicitly recovered task or left unknown usage")
	}
	if _, err = store.Recover(run.Workspace, run.ID, item.ID, "authoritative receipt matched; explicitly retry", false); err != nil {
		t.Fatal(err)
	}
	replacement, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "replacement-worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	settled, err = store.Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	nextCtx := coordinatorUsageContext(context.Background(), store, run.Workspace, run.ID, settled, replacement, replacement.Owner, "fixture-provider", budget.NewTracker(0, budget.Rates{InputPerMTok: 1, OutputPerMTok: 2}))
	response, err := ordinaryUsagePost(t, nextCtx, transport.NewHTTPClient(time.Second, "openai"), endpoint, "explicit recovery after matched receipt")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("replacement completion: %v %v", err, closeErr)
	}
	final, err := store.Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || len(final.UsageRequests) != 2 || len(projectcoord.UnresolvedUsageRequests(final)) != 0 || final.UsageRequests[1].Status != projectcoord.UsageRequestCompleted {
		t.Fatalf("receipt/recovery boundary failed: posts=%d records=%+v", requests.Load(), final.UsageRequests)
	}
}

func TestOrdinaryCoordinatorParentCancelCannotAutoRecoverUnknownProviderUsage(t *testing.T) {
	store, run, item := ordinaryUsageFixture(t)
	owner := &coordinatorUsageObserver{store: store, workspace: run.Workspace, runID: run.ID, itemID: item.ID, worker: item.Owner, token: item.Lease.Token, provider: "fixture-provider", tracker: budget.NewTracker(0, budget.Rates{})}
	id, err := projectcoord.NewUsageRequestID()
	if err != nil {
		t.Fatal(err)
	}
	request, err := owner.Begin(context.Background(), transport.RequestMetadata{ClientRequestID: id, Provider: "openai", Model: "fixture-model", EndpointSHA256: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err = request.End(transport.UsageInterrupted); err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	failure := coordinatorWorkerFailure(parent, context.Canceled)
	result, err := recordCoordinatorFailure(store, run.Workspace, run.ID, item, item.Owner, failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != projectcoord.ItemFailed || result.Failure == nil || result.Failure.Code != "provider_usage_unresolved" || result.Failure.AutoRecovered || result.Failure.Recoverable {
		t.Fatalf("service cancellation made unsettled work claimable: %+v", result)
	}
	if _, err = store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "replacement", Lease: time.Minute}); !errors.Is(err, projectcoord.ErrNoReadyWork) {
		t.Fatalf("unsettled parent-cancelled work re-claimed: %v", err)
	}
}

func TestOrdinaryCoordinatorLiveRequestsWaitForSettlementAndWaitingContextCancels(t *testing.T) {
	store, run, item := ordinaryUsageFixture(t)
	owner := &coordinatorUsageObserver{store: store, workspace: run.Workspace, runID: run.ID, itemID: item.ID, worker: item.Owner, token: item.Lease.Token, provider: "fixture-provider", tracker: budget.NewTracker(0, budget.Rates{})}
	metadata := func() transport.RequestMetadata {
		id, err := projectcoord.NewUsageRequestID()
		if err != nil {
			t.Fatal(err)
		}
		return transport.RequestMetadata{ClientRequestID: id, Provider: "openai", Model: "fixture-model", EndpointSHA256: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64)}
	}
	first, err := owner.Begin(context.Background(), metadata())
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Snapshot(transport.UsageSnapshot{UsageReported: true, InputTokens: 1, OutputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = owner.Begin(waitCtx, metadata())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("live request did not respect waiting caller cancellation: %v", err)
	}
	type result struct {
		r   transport.RequestUsageObserver
		err error
	}
	next := make(chan result, 1)
	meta := metadata()
	go func() { r, err := owner.Begin(context.Background(), meta); next <- result{r, err} }()
	select {
	case got := <-next:
		t.Fatalf("parallel request bypassed live gate: %v", got.err)
	case <-time.After(30 * time.Millisecond):
	}
	if err = first.End(transport.UsageCompleted); err != nil {
		t.Fatal(err)
	}
	var admitted result
	select {
	case admitted = <-next:
	case <-time.After(2 * time.Second):
		t.Fatal("settled request did not release live dispatch gate")
	}
	if admitted.err != nil || admitted.r == nil {
		t.Fatalf("normal parallel request was falsely blocked as unknown: %v", admitted.err)
	}
	if err = admitted.r.Snapshot(transport.UsageSnapshot{UsageReported: true, InputTokens: 2, OutputTokens: 2}); err != nil {
		t.Fatal(err)
	}
	if err = admitted.r.End(transport.UsageCompleted); err != nil {
		t.Fatal(err)
	}
	final, err := store.Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.UsageRequests) != 2 || len(projectcoord.UnresolvedUsageRequests(final)) != 0 {
		t.Fatalf("normal serialized requests did not settle: %+v", final.UsageRequests)
	}
}

func TestOrdinaryCoordinatorBeginFailureReleasesLiveGate(t *testing.T) {
	store, run, item := ordinaryUsageFixture(t)
	owner := &coordinatorUsageObserver{store: store, workspace: run.Workspace, runID: run.ID, itemID: item.ID, worker: item.Owner, token: item.Lease.Token, provider: "fixture-provider", tracker: budget.NewTracker(0, budget.Rates{})}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := owner.Begin(ctx, transport.RequestMetadata{}); err == nil {
		t.Fatal("invalid request was admitted")
	}
	id, err := projectcoord.NewUsageRequestID()
	if err != nil {
		t.Fatal(err)
	}
	r, err := owner.Begin(ctx, transport.RequestMetadata{ClientRequestID: id, Provider: "openai", Model: "fixture-model", EndpointSHA256: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatalf("failed Begin stranded dispatch gate: %v", err)
	}
	if err = r.End(transport.UsageInterrupted); err != nil {
		t.Fatal(err)
	}
}

func TestOrdinaryCoordinatorDetachedStartupHelperWaitsForIdentityAndEndsWithExecution(t *testing.T) {
	store, run, item := ordinaryUsageFixture(t)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"helper-response","model":"physical-helper-model","usage":{"prompt_tokens":17,"completion_tokens":3},"choices":[{"message":{"role":"assistant","content":"helper done"}}]}`)
	}))
	defer server.Close()
	execution, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, owner := coordinatorExecutionUsageContext(execution, store, run.Workspace, run.ID, run, item, item.Owner, nil)
	if owner.tracker == nil {
		t.Fatal("startup helper has no durable budget tracker")
	}
	provider := llm.BindExecutionProvider(openai.New("private-helper-test-key", server.URL+"/v1", "physical-helper-model", 128, time.Second, 0), ctx)
	finished := make(chan error, 1)
	go func() {
		_, err := provider.Complete(context.Background(), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "startup memory helper"}}}}})
		finished <- err
	}()
	select {
	case err := <-finished:
		t.Fatalf("helper passed unresolved identity barrier: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if posts.Load() != 0 {
		t.Fatal("startup helper dispatched under a guessed provider account")
	}
	owner.bindExecutionIdentity("actual-selected-account", "parent-model")
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("identity-ready helper was stranded")
	}
	final, err := store.Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.UsageRequests) != 1 || final.UsageRequests[0].Provider != "actual-selected-account" || final.UsageRequests[0].Model != "physical-helper-model" || !final.UsageRequests[0].UsageReported || final.UsageRequests[0].Usage.Input != 17 || len(projectcoord.UnresolvedUsageRequests(final)) != 0 {
		t.Fatalf("detached helper escaped execution accounting: %+v", final.UsageRequests)
	}
	if final.UsageRequests[0].Prices.Source != "physical request model pricing unavailable" {
		t.Fatal("unpriced helper model was labeled as a known free model")
	}
	cancel()
	if _, err = provider.Complete(context.Background(), llm.Request{}); !errors.Is(err, context.Canceled) || posts.Load() != 1 {
		t.Fatalf("cleanup-ended helper dispatched again: posts=%d err=%v", posts.Load(), err)
	}
}

func TestOrdinaryCoordinatorStartupIdentityWaitCancelsAndCannotWaitForever(t *testing.T) {
	store, run, item := ordinaryUsageFixture(t)
	_, owner := coordinatorExecutionUsageContext(context.Background(), store, run.Workspace, run.ID, run, item, item.Owner, nil)
	id, err := projectcoord.NewUsageRequestID()
	if err != nil {
		t.Fatal(err)
	}
	meta := transport.RequestMetadata{ClientRequestID: id, Provider: "openai", Model: "fixture-model", EndpointSHA256: strings.Repeat("a", 64), RequestSHA256: strings.Repeat("b", 64)}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = owner.Begin(cancelled, meta); !errors.Is(err, context.Canceled) {
		t.Fatalf("startup identity barrier ignored caller cancellation: %v", err)
	}
	owner.identityWait = 25 * time.Millisecond
	if _, err = owner.Begin(context.Background(), meta); err == nil {
		t.Fatal("synchronous startup helper waited indefinitely for its own runtime setup")
	}
	current, err := store.Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.UsageRequests) != 0 {
		t.Fatal("unresolved startup identity created an unowned provider reservation")
	}
}

func TestOrdinaryCoordinatorMalformedHeaderReleasesGateAndKeepsUsageUnknown(t *testing.T) {
	store, run, item := ordinaryUsageFixture(t)
	ctx := coordinatorUsageContext(context.Background(), store, run.Workspace, run.ID, run, item, item.Owner, "fixture-provider", budget.NewTracker(0, budget.Rates{}))
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("request-id", "invalid request identity with whitespace")
		_, _ = io.WriteString(w, `{"error":"private-response-body"}`)
	}))
	defer server.Close()
	client := transport.NewHTTPClient(time.Second, "openai")
	if response, err := ordinaryUsagePost(t, ctx, client, server.URL, "first"); err == nil {
		_ = response.Body.Close()
		t.Fatal("invalid provider identity was accepted")
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err := ordinaryUsagePost(t, waitCtx, client, server.URL, "must not retry unknown usage")
	var boundary *transport.UsageObserverError
	if !errors.As(err, &boundary) || boundary.Code != "provider_usage_unresolved" || posts.Load() != 1 {
		t.Fatalf("header failure stranded gate or allowed retry: posts=%d err=%v", posts.Load(), err)
	}
}
