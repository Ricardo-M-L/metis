package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/billing"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

func coordinatorBillingFixture(t *testing.T, endpoint, provider, model string, withoutRemoteIDs ...bool) coordinatorUsageFixture {
	t.Helper()
	requireCoordinatorUsagePrivateFiles(t)
	home, workspace := t.TempDir(), t.TempDir()
	store := projectcoord.NewStore(filepath.Join(home, "store"))
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "native bill recovery", Items: []projectcoord.WorkItemInput{{ID: "work", Subject: "Work", Prompt: "Do work"}}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := projectcoord.NewUsageRequestID()
	hash, _ := billing.EndpointSHA256(endpoint)
	record, err := store.BeginUsageRequest(run.Workspace, run.ID, projectcoord.BeginUsageRequestInput{ClientRequestID: id, ItemID: item.ID, Worker: item.Owner, ClaimToken: item.Lease.Token, Provider: provider, EndpointSHA256: hash, Model: model, RequestSHA256: strings.Repeat("b", 64), Prices: projectcoord.UsagePriceSnapshot{InputPerMillionUSD: 10, OutputPerMillionUSD: 20, Source: "catalogue-estimate"}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := projectcoord.UsageSnapshotInput{ClientRequestID: id, ItemID: item.ID, Worker: item.Owner, ClaimToken: item.Lease.Token, ProviderRequestID: "request-1", ResponseID: "gen-test-1", Status: projectcoord.UsageRequestCompleted, UsageReported: true, Usage: projectcoord.TokenUsage{Input: 100, Output: 20}}
	if len(withoutRemoteIDs) > 0 && withoutRemoteIDs[0] {
		snapshot.ProviderRequestID = ""
		snapshot.ResponseID = ""
		snapshot.Status = projectcoord.UsageRequestInterrupted
		snapshot.UsageReported = false
		snapshot.Usage = projectcoord.TokenUsage{}
	}
	_, err = store.RecordUsageSnapshot(run.Workspace, run.ID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return coordinatorUsageFixture{store: store, workspace: run.Workspace, home: home, run: run, record: record}
}

func TestCoordinatorNativeCLIProxyGUIRecoversOrphanBeforeHeadersByExactClientMarker(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 unavailable")
	}
	endpoint := "http://127.0.0.1:8317/v1/responses"
	f := coordinatorBillingFixture(t, endpoint, "easycliproxyapi", "gpt-6-sol", true)
	db := filepath.Join(f.home, "usage.db")
	query := `CREATE TABLE usage_events(event_key TEXT,request_id TEXT,provider TEXT,model TEXT,alias TEXT,user_agent TEXT,failed INTEGER,canceled INTEGER,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_creation_tokens INTEGER); INSERT INTO usage_events VALUES('event1','recovered-gateway-id','codex','gpt-6-sol','gpt-6-sol','Go-http-client/1.1 metis-usage/` + f.record.ClientRequestID + `',0,0,100,20,30,0);`
	if out, err := exec.Command(sqlite, db, query).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	config := coordinatorBillingWriteConfig(t, f, coordinatorBillingConfig{Provider: "cliproxy_gui", LedgerProvider: "easycliproxyapi", InferenceEndpoint: endpoint, UsageDB: db, SQLiteExecutable: sqlite})
	r, err := newCoordinatorUsageReconciler(f.store, f.workspace, f.run.ID, f.home, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.configureBilling(config); err != nil {
		t.Fatal(err)
	}
	summary, err := r.CheckOnce(context.Background())
	if err != nil || summary.Unresolved != 0 || summary.Imported != 1 || summary.ActualCostUnknownRequests != 1 {
		t.Fatalf("orphan not recovered %+v %v", summary, err)
	}
	run, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil || run.UsageRequests[0].ProviderRequestID != "recovered-gateway-id" || run.UsageRequests[0].ActualUSD != nil || run.UsageRequests[0].Usage.Input != 70 {
		t.Fatal("lost exact request correlation or fabricated cost")
	}
}

func coordinatorBillingWriteConfig(t *testing.T, f coordinatorUsageFixture, config coordinatorBillingConfig) string {
	t.Helper()
	path := filepath.Join(f.home, "billing-config.json")
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCoordinatorNativeBillingSettlesCompletedRequestsPersistsRawAndReplays(t *testing.T) {
	cost := "0.002"
	available := false
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !available {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"data":{"id":"gen-test-1","request_id":"request-1","model":"test/model","finish_reason":"stop","is_byok":false,"native_tokens_prompt":100,"native_tokens_completion":20,"native_tokens_cached":30,"total_cost":` + cost + `}}`))
	}))
	defer server.Close()
	f := coordinatorBillingFixture(t, server.URL+"/chat/completions", "my-openrouter", "test/model")
	tokenFile := filepath.Join(f.home, "key")
	if err := os.WriteFile(tokenFile, []byte("host-secret-not-in-evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	config := coordinatorBillingWriteConfig(t, f, coordinatorBillingConfig{Provider: "openrouter", LedgerProvider: "my-openrouter", InferenceEndpoint: server.URL + "/chat/completions", APIBaseURL: server.URL, TokenFile: tokenFile})
	r, err := newCoordinatorUsageReconciler(f.store, f.workspace, f.run.ID, f.home, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.configureBilling(config); err != nil {
		t.Fatal(err)
	}
	summary, err := r.CheckOnce(context.Background())
	if err != nil || summary.Unresolved != 0 || summary.ActualCostUnknownRequests != 1 || summary.Imported != 0 {
		t.Fatalf("delayed bill falsely settled %+v %v", summary, err)
	}
	available = true
	r.billing.next = map[string]time.Time{}
	summary, err = r.CheckOnce(context.Background())
	if err != nil || summary.ActualKnownUSD != .002 || summary.ActualCostUnknownRequests != 0 || summary.Imported != 1 {
		t.Fatalf("completed request not billed %+v %v", summary, err)
	}
	evidence, err := os.ReadDir(r.billing.evidenceDir)
	if err != nil || len(evidence) != 1 {
		t.Fatal("lost raw provider evidence")
	}
	data, err := os.ReadFile(filepath.Join(r.billing.evidenceDir, evidence[0].Name()))
	if err != nil || strings.Contains(string(data), "host-secret") {
		t.Fatal("credential leaked or evidence missing")
	}
	// A new service process obtains the same record. It must not charge again.
	restarted, err := newCoordinatorUsageReconciler(f.store, f.workspace, f.run.ID, f.home, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.configureBilling(config); err != nil {
		t.Fatal(err)
	}
	if summary, err = restarted.CheckOnce(context.Background()); err != nil || summary.Imported != 0 || summary.ActualKnownUSD != .002 {
		t.Fatalf("restart double counted %+v %v", summary, err)
	}
	// Provider corrections can lower actual costs only by superseding the
	// current receipt. A stale original record cannot later roll them back.
	cost = "0.001"
	restarted.billing.next = map[string]time.Time{}
	if summary, err = restarted.CheckOnce(context.Background()); err != nil || summary.Imported != 1 || summary.ActualKnownUSD != .001 {
		t.Fatalf("correction failed %+v %v", summary, err)
	}
	cost = "0.002"
	restarted.billing.next = map[string]time.Time{}
	if summary, err = restarted.CheckOnce(context.Background()); err != nil || summary.Imported != 0 || summary.ActualKnownUSD != .001 {
		t.Fatalf("old revision rolled back newer charge %+v %v", summary, err)
	}
	run, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil || len(run.UsageRequests[0].Receipts) != 2 || run.UsageRequests[0].Receipts[1].SupersedesReceiptID != run.UsageRequests[0].Receipts[0].ReceiptID {
		t.Fatal("billing revisions were not durable")
	}
	if calls != 5 {
		t.Fatalf("unexpected lookup count %d", calls)
	}
}

func TestCoordinatorNativeBillingCannotCrossEndpointScopeOrReadWorkspaceKey(t *testing.T) {
	f := coordinatorBillingFixture(t, "https://openrouter.ai/api/v1/chat/completions", "openrouter", "test/model")
	path := filepath.Join(f.workspace, "key")
	if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	config := coordinatorBillingWriteConfig(t, f, coordinatorBillingConfig{Provider: "openrouter", LedgerProvider: "openrouter", InferenceEndpoint: "https://openrouter.ai/api/v1/chat/completions", TokenFile: path})
	r, err := newCoordinatorUsageReconciler(f.store, f.workspace, f.run.ID, f.home, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.configureBilling(config); err == nil {
		t.Fatal("accepted model-writable billing secret")
	}
	if err := os.Chmod(config, 0644); err != nil {
		t.Fatal(err)
	}
	if err := r.configureBilling(config); err == nil {
		t.Fatal("accepted public billing config")
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
	defer server.Close()
	path = filepath.Join(f.home, "private-key")
	if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	config = coordinatorBillingWriteConfig(t, f, coordinatorBillingConfig{Provider: "openrouter", LedgerProvider: "openrouter", InferenceEndpoint: server.URL + "/chat/completions", APIBaseURL: server.URL, TokenFile: path})
	if err := r.configureBilling(config); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CheckOnce(context.Background()); err != nil || calls != 0 {
		t.Fatal("queried another endpoint's request")
	}
}

func TestCoordinatorNativeCLIProxyGUISettlesTokensAndKeepsUnknownMoney(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 unavailable")
	}
	endpoint := "http://127.0.0.1:8317/v1/responses"
	f := coordinatorBillingFixture(t, endpoint, "easycliproxyapi", "gpt-6-sol")
	db := filepath.Join(f.home, "usage.db")
	query := `CREATE TABLE usage_events(event_key TEXT,request_id TEXT,provider TEXT,model TEXT,alias TEXT,user_agent TEXT,failed INTEGER,canceled INTEGER,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_creation_tokens INTEGER); INSERT INTO usage_events VALUES('event1','request-1','codex','gpt-6-sol','gpt-6-sol','metis-usage/` + f.record.ClientRequestID + `',0,0,100,20,30,0);`
	if out, err := exec.Command(sqlite, db, query).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	config := coordinatorBillingWriteConfig(t, f, coordinatorBillingConfig{Provider: "cliproxy_gui", LedgerProvider: "easycliproxyapi", InferenceEndpoint: endpoint, UsageDB: db, SQLiteExecutable: sqlite})
	r, err := newCoordinatorUsageReconciler(f.store, f.workspace, f.run.ID, f.home, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.configureBilling(config); err != nil {
		t.Fatal(err)
	}
	summary, err := r.CheckOnce(context.Background())
	if err != nil || summary.Imported != 1 || summary.ActualCostUnknownRequests != 1 || summary.ActualKnownUSD != 0 {
		t.Fatalf("gateway tokens were called actual dollars %+v %v", summary, err)
	}
	run, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil || run.UsageRequests[0].ActualUSD != nil || run.UsageRequests[0].Usage.CacheRead != 30 {
		t.Fatal("token-only recovery corrupted ledger")
	}
	r.billing.next = map[string]time.Time{}
	if summary, err = r.CheckOnce(context.Background()); err != nil || summary.Imported != 0 {
		t.Fatal("repeated GUI record created another receipt")
	}
}

func TestCoordinatorNativeBillingProjectsLateReceiptForArchivedJobRun(t *testing.T) {
	requireCoordinatorUsagePrivateFiles(t)
	job, store, run, item := newPersistentJobCLIFixture(t, 0)
	requestID := strings.Repeat("a", 32)
	_, err := store.BeginUsageRequest(run.Workspace, run.ID, projectcoord.BeginUsageRequestInput{ClientRequestID: requestID, ItemID: item.ID, Worker: item.Owner, ClaimToken: item.Lease.Token, Provider: "fixture", EndpointSHA256: strings.Repeat("b", 64), Model: "model", RequestSHA256: strings.Repeat("c", 64), Prices: projectcoord.UsagePriceSnapshot{Source: "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.RecordUsageSnapshot(run.Workspace, run.ID, projectcoord.UsageSnapshotInput{ClientRequestID: requestID, ItemID: item.ID, Worker: item.Owner, ClaimToken: item.Lease.Token, Status: projectcoord.UsageRequestCompleted, UsageReported: true, Usage: projectcoord.TokenUsage{Input: 10, Output: 2}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Complete(context.Background(), run.Workspace, run.ID, item.ID, projectcoord.CompletionInput{Worker: item.Owner, Token: item.Lease.Token, Output: "completed with verified evidence"})
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.Get(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinatorBillingProjectJobUsage(run); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < principal.HotRunHistoryLimit; i++ {
		id := fmt.Sprintf("history-%03d", i)
		if _, err := persistentJobStore().RegisterRun(job.Binding(), id); err != nil {
			t.Fatal(err)
		}
		if _, err := persistentJobStore().RecordRunUsage(job.Binding(), principal.RunUsage{RunID: id, SourceRevision: 1, Status: "completed"}); err != nil {
			t.Fatal(err)
		}
	}
	job, err = persistentJobStore().Get(job.ID)
	if err != nil || job.RunHistory.Count != 1 {
		t.Fatal("test did not archive original completed run")
	}
	refresh, err := persistentJobStore().RefreshRuns(job.ID)
	if err != nil || len(refresh) != 0 {
		t.Fatal("archived run still appeared in active refresh")
	}
	r, err := newCoordinatorUsageReconciler(store, run.Workspace, run.ID, config.Home(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	actual := .09
	receipt := projectcoord.UsageReceipt{ReceiptID: "late-bill", EvidenceSHA256: strings.Repeat("d", 64), Authority: "trusted-test-export", Provider: "fixture", EndpointSHA256: strings.Repeat("b", 64), Model: "model", ClientRequestID: requestID, Usage: projectcoord.TokenUsage{Input: 20, Output: 3}, ActualUSD: &actual}
	coordinatorUsageWriteReceipt(t, filepath.Join(r.receiptDir, "late.json"), receipt)
	if _, err := r.CheckOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, err = persistentJobStore().Get(job.ID)
	if err != nil || job.SpentUSD != .09 || job.InputTokens != 20 || job.OutputTokens != 3 || job.RunHistory.Count != 1 {
		t.Fatalf("archived lifetime cost did not receive bill: %+v %v", job, err)
	}
	if _, err := r.CheckOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	again, err := persistentJobStore().Get(job.ID)
	if err != nil || again.SpentUSD != .09 || again.InputTokens != 20 {
		t.Fatal("late archive projection was not idempotent")
	}
}
