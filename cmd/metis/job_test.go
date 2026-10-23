package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

func newPersistentJobCLIFixture(t *testing.T, maxInput int64) (principal.Job, *projectcoord.Store, projectcoord.Run, projectcoord.WorkItem) {
	t.Helper()
	requireCoordinatorPOSIXTest(t, "persistent job storage requires POSIX directory fsync")
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	workspace, err := projectCoordinatorWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "config.toml"), []byte("[provider]\ndefault=\"fixture\"\n[provider.custom.fixture]\ntransport=\"anthropic_messages\"\nbase_url=\"https://example.invalid/v1\"\nmodel=\"model\"\napi_key_env=\"JOB_TEST_KEY\"\n[permission]\nmode=\"plan\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(workspace, ".metis", "agents")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "reviewer.md"), []byte("---\nname: reviewer\ntools: Read\npermission_mode: plan\n---\nFROZEN_JOB_ROLE\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--name", "stable", "--cwd", workspace, "--agent", "reviewer", "--json"}
	if maxInput > 0 {
		args = append(args, "--max-input-tokens", "1")
	}
	output, err := captureCoordinatorOperationOutput(t, func() error { return cmdJobCreate(args) })
	if err != nil {
		t.Fatal(err)
	}
	var j principal.Job
	if err = json.Unmarshal([]byte(output), &j); err != nil {
		t.Fatal(err)
	}
	b := j.Binding()
	store := projectCoordinatorStore()
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "test", Items: []projectcoord.WorkItemInput{{ID: "work", Subject: "work", Prompt: "work", AgentProfile: "reviewer", PrincipalBinding: &b}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = persistentJobStore().RegisterRun(b, run.ID); err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(workspace, run.ID, projectcoord.ClaimOptions{Worker: "fixture-worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return j, store, run, item
}

func TestOrphanPrincipalRunIsRegisteredBeforeExecutionAndUnknownUsageBlocksIt(t *testing.T) {
	j, store, run, item := newPersistentJobCLIFixture(t, 0)
	// Simulate a crash after the coordinator journal committed the binding but
	// before the independent principal registration file was synced.
	j.Runs = nil
	j.SpentUSD = 0
	j.InputTokens = 0
	j.OutputTokens = 0
	j.UnresolvedRequests = 0
	data, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(config.Home(), "jobs", j.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = store.BeginUsageRequest(run.Workspace, run.ID, projectcoord.BeginUsageRequestInput{ClientRequestID: strings.Repeat("a", 32), ItemID: item.ID, Worker: item.Owner, ClaimToken: item.Lease.Token, Provider: "fixture", Model: "model", EndpointSHA256: strings.Repeat("b", 64), RequestSHA256: strings.Repeat("c", 64), Prices: projectcoord.UsagePriceSnapshot{Source: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	err = inJobWorkspace(j.Definition.Workspace, func() error {
		_, _, _, stop, err := preparePersistentJobWorker(context.Background(), store, run, item, item.Owner, time.Minute)
		defer stop()
		return err
	})
	if !errors.Is(err, principal.ErrUsageUnknown) {
		t.Fatalf("orphan provider usage bypassed lifetime budget: %v", err)
	}
	updated, err := persistentJobStore().Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Runs) != 1 || updated.Runs[0].RunID != run.ID || updated.UnresolvedRequests != 1 || updated.Lease != nil {
		t.Fatalf("orphan registration did not recover accounting safely: %+v", updated)
	}
}

func TestUSDJobRejectsUnknownPricingBeforePhysicalReservation(t *testing.T) {
	j, store, _, _ := newPersistentJobCLIFixture(t, 0)
	d := j.Definition
	d.Budget.MaxUSD = 1
	priced, err := persistentJobStore().Create("usd limited", d)
	if err != nil {
		t.Fatal(err)
	}
	binding := priced.Binding()
	run, err := store.Create(projectcoord.CreateInput{Workspace: d.Workspace, Goal: "price guard", Items: []projectcoord.WorkItemInput{{ID: "work", Subject: "work", Prompt: "work", AgentProfile: d.Profile.Name, PrincipalBinding: &binding}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = persistentJobStore().RegisterRun(binding, run.ID); err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "fixture-worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := persistentJobStore().ClaimExecution(binding, "execution", "fixture-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	o := &persistentJobUsageObserver{delegate: &coordinatorUsageObserver{store: store, workspace: run.Workspace, runID: run.ID, itemID: item.ID, worker: item.Owner, token: item.Lease.Token, provider: d.Provider, tracker: budget.NewTracker(1, budget.Rates{})}, store: store, binding: binding, lease: lease, gate: make(chan struct{}, 1)}
	o.gate <- struct{}{}
	_, err = o.Begin(context.Background(), transport.RequestMetadata{ClientRequestID: strings.Repeat("a", 32), Provider: "anthropic", Model: "model", EndpointSHA256: strings.Repeat("b", 64), RequestSHA256: strings.Repeat("c", 64)})
	if err == nil || !strings.Contains(err.Error(), "known model pricing") {
		t.Fatalf("unpriced request passed USD-only budget: %v", err)
	}
	current, err := store.Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.UsageRequests) != 0 {
		t.Fatal("unknown-price request reached dispatch reservation")
	}
}

func TestPrincipalPauseAndLostLeaseUseBoundedWorkerRecovery(t *testing.T) {
	for _, err := range []error{principal.ErrInactive, principal.ErrLeaseLost} {
		f := coordinatorWorkerFailure(context.Background(), err)
		if f.Code != projectcoord.FailureWorkerInterrupted {
			t.Fatalf("principal lifecycle failure became permanent: %+v", f)
		}
	}
	uncertain := errors.Join(principal.ErrInactive, projectcoord.ErrOperationUncertain)
	if f := coordinatorWorkerFailure(context.Background(), uncertain); f.Code != projectcoord.FailureOperationUncertain {
		t.Fatal("pause masked unresolved operation")
	}
}

func TestUnsettledHTTPRejectionHasSafeHostFailureAndNeverDispatchesRetry(t *testing.T) {
	j, store, run, item := newPersistentJobCLIFixture(t, 0)
	lease, err := persistentJobStore().ClaimExecution(j.Binding(), "execution", item.Owner, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tracker := budget.NewTracker(0, budget.Rates{})
	ctx := coordinatorPersistentJobUsageContext(context.Background(), store, run.Workspace, run.ID, run, item, item.Owner, j.Definition.Provider, tracker, lease)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"private-gateway-diagnostic-and-key"}`)
	}))
	defer server.Close()
	client := transport.NewHTTPClient(5*time.Second, "openai")
	post := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(`{"model":"model","stream":false}`))
		if err != nil {
			t.Fatal(err)
		}
		return client.Do(req)
	}
	response, err := post()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	_, err = post()
	var observerErr *transport.UsageObserverError
	if !errors.As(err, &observerErr) || !errors.Is(observerErr.Err, principal.ErrUsageUnknown) {
		t.Fatalf("expected explicit unknown-usage guard before retry: %v", err)
	}
	if requests != 1 {
		t.Fatalf("unknown consumption allowed %d provider POSTs", requests)
	}
	failure := coordinatorWorkerFailure(context.Background(), err)
	if failure.Code != "provider_usage_unresolved" || failure.Recoverable || failure.AutoRecovered || strings.Contains(failure.Summary, "private-") {
		t.Fatalf("host failure lost safe actionable classification: %+v", failure)
	}
	updated, err := store.Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(updated.UsageRequests[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["response_status_code"] != float64(http.StatusForbidden) || updated.UsageRequests[0].UsageReported || len(updated.UsageRequests) != 1 {
		t.Fatalf("HTTP status diagnostic changed unknown-consumption policy: %s", data)
	}
	public := coordinatorUsagePublic(updated.UsageRequests[0])
	if public.ResponseStatusCode != http.StatusForbidden {
		t.Fatal("safe host usage listing lost original HTTP status")
	}
	publicJSON, _ := json.Marshal(public)
	if strings.Contains(string(publicJSON), item.Lease.Token) || strings.Contains(string(publicJSON), "private-gateway") {
		t.Fatal("HTTP status diagnostic exposed worker credentials or response body")
	}
	if _, err = store.Fail(run.Workspace, run.ID, item.ID, item.Owner, item.Lease.Token, failure, ""); err != nil {
		t.Fatal(err)
	}
	updated, err = store.Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != projectcoord.RunBlocked || updated.Items[0].Status == projectcoord.ItemReady {
		t.Fatalf("unknown usage became automatic recovery: %+v", updated.Items[0])
	}
}

func TestPersistentJobCLIFreezesRoleAndResumeIdentity(t *testing.T) {
	j, _, _, item := newPersistentJobCLIFixture(t, 0)
	path := filepath.Join(j.Definition.Workspace, ".metis", "agents", "reviewer.md")
	if err := os.WriteFile(path, []byte("---\nname: reviewer\ntools: Bash\npermission_mode: fullAccess\n---\nMUTATED_JOB_ROLE\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := persistentJobRuntimeFlags(item, j)
	if err != nil {
		t.Fatal(err)
	}
	if f.agentProfileSnapshot.SystemPrompt != "FROZEN_JOB_ROLE" || f.agentProfileSnapshot.Tools[0] != "Read" || f.agentProfileSnapshot.PermissionMode != "plan" || f.newSessionID != j.SessionID || f.jobSessionDir != j.SessionDir || f.jobMemoryDir != j.MemoryDir || f.jobID != j.ID {
		t.Fatalf("runtime ignored frozen principal: %+v", f)
	}
	if err = os.MkdirAll(j.SessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(j.SessionDir, j.SessionID+".jsonl"), []byte("history"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err = persistentJobRuntimeFlags(item, j)
	if err != nil {
		t.Fatal(err)
	}
	if f.resumeID != j.SessionID || f.newSessionID != "" {
		t.Fatal("replacement worker starts a different transcript")
	}
}

func TestJobProviderFingerprintAllowsKeyRotationButRejectsRouteAndPermissionDrift(t *testing.T) {
	cfg := &config.Config{Provider: config.ProviderSet{Custom: map[string]config.ProviderRaw{"fixture": {Transport: "anthropic_messages", BaseURL: "https://example.invalid", Model: "model", APIKey: "secret-one", APIKeyEnv: "BOUND_KEY"}}}, Permission: config.Permission{Mode: "plan"}}
	original, _, _, err := jobProviderFingerprint(cfg, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Provider.Custom["fixture"]
	p.APIKey = "secret-two"
	cfg.Provider.Custom["fixture"] = p
	rotated, _, _, err := jobProviderFingerprint(cfg, "fixture")
	if err != nil || rotated != original {
		t.Fatal("secret rotation altered non-secret route identity")
	}
	p.BaseURL = "https://different.invalid"
	cfg.Provider.Custom["fixture"] = p
	changed, _, _, _ := jobProviderFingerprint(cfg, "fixture")
	if changed == original {
		t.Fatal("endpoint switch did not invalidate frozen route")
	}
	p.BaseURL = "https://example.invalid"
	cfg.Provider.Custom["fixture"] = p
	cfg.Permission.Mode = "fullAccess"
	changed, _, _, _ = jobProviderFingerprint(cfg, "fixture")
	if changed == original {
		t.Fatal("host privilege change did not invalidate frozen job")
	}
}

func TestJobTransportObserverPersistsPhysicalUsageAndRejectsNextDispatchAtLifetimeCap(t *testing.T) {
	j, store, run, item := newPersistentJobCLIFixture(t, 1)
	lease, err := persistentJobStore().ClaimExecution(j.Binding(), "execution", "fixture-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	o := &persistentJobUsageObserver{delegate: &coordinatorUsageObserver{store: store, workspace: run.Workspace, runID: run.ID, itemID: item.ID, worker: item.Owner, token: item.Lease.Token, provider: "fixture", tracker: rtpkg.NewBudgetTracker(0, "model")}, store: store, binding: j.Binding(), lease: lease, gate: make(chan struct{}, 1)}
	o.gate <- struct{}{}
	meta := transport.RequestMetadata{ClientRequestID: strings.Repeat("a", 32), Provider: "anthropic", Model: "model", EndpointSHA256: strings.Repeat("b", 64), RequestSHA256: strings.Repeat("c", 64)}
	r, err := o.Begin(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Snapshot(transport.UsageSnapshot{UsageReported: true, InputTokens: 10, OutputTokens: 2, CacheReadInputTokens: 20}); err != nil {
		t.Fatal(err)
	}
	if err = r.End(transport.UsageCompleted); err != nil {
		t.Fatal(err)
	}
	updated, err := persistentJobStore().Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.InputTokens != 30 || updated.OutputTokens != 2 || updated.UnresolvedRequests != 0 {
		t.Fatalf("usage did not settle under principal: %+v", updated)
	}
	meta.ClientRequestID = strings.Repeat("d", 32)
	if _, err = o.Begin(context.Background(), meta); !errors.Is(err, principal.ErrBudget) {
		t.Fatalf("next physical dispatch bypassed lifetime cap: %v", err)
	}
	current, err := store.Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.UsageRequests) != 1 {
		t.Fatal("rejected request was dispatched or reserved")
	}
}

func TestJobUsageInterruptedBeforeReceiptCannotBeErasedByReplacementWorker(t *testing.T) {
	j, store, run, item := newPersistentJobCLIFixture(t, 0)
	_, err := store.BeginUsageRequest(run.Workspace, run.ID, projectcoord.BeginUsageRequestInput{ClientRequestID: strings.Repeat("a", 32), ItemID: item.ID, Worker: item.Owner, ClaimToken: item.Lease.Token, Provider: "fixture", Model: "model", EndpointSHA256: strings.Repeat("b", 64), RequestSHA256: strings.Repeat("c", 64), Prices: projectcoord.UsagePriceSnapshot{Source: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = syncPersistentJobUsage(store, j.Binding()); err != nil {
		t.Fatal(err)
	}
	updated, err := persistentJobStore().Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.UnresolvedRequests != 1 {
		t.Fatal("started request was silently treated as zero usage")
	}
	if _, err = persistentJobStore().ClaimExecution(j.Binding(), "replacement", "worker", time.Minute); !errors.Is(err, principal.ErrUsageUnknown) {
		t.Fatalf("new worker ignored unresolved prior request: %v", err)
	}
}
