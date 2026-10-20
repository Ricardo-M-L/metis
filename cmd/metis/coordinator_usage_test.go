package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

type coordinatorUsageFixture struct {
	store           *projectcoord.Store
	workspace, home string
	run             projectcoord.Run
	record          projectcoord.UsageRequestRecord
}

func newCoordinatorUsageFixture(t *testing.T) coordinatorUsageFixture {
	t.Helper()
	workspace, home := t.TempDir(), t.TempDir()
	store := projectcoord.NewStore(filepath.Join(home, "project-coordinator"))
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "Usage receipt checks", Items: []projectcoord.WorkItemInput{{ID: "work", Subject: "Work", Prompt: "Do work"}}})
	if err != nil {
		t.Fatal(err)
	}
	workspace = run.Workspace
	item, err := store.ClaimNext(workspace, run.ID, projectcoord.ClaimOptions{Worker: "original", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	id, err := projectcoord.NewUsageRequestID()
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.BeginUsageRequest(workspace, run.ID, projectcoord.BeginUsageRequestInput{ClientRequestID: id, ItemID: item.ID, Worker: item.Owner, ClaimToken: item.Lease.Token, Provider: "selected-provider", EndpointSHA256: strings.Repeat("a", 64), Model: "selected-model", RequestSHA256: strings.Repeat("b", 64), Prices: projectcoord.UsagePriceSnapshot{InputPerMillionUSD: 2, OutputPerMillionUSD: 4, Source: "catalogue-test"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.RecordUsageSnapshot(workspace, run.ID, projectcoord.UsageSnapshotInput{ClientRequestID: id, ItemID: item.ID, Worker: item.Owner, ClaimToken: item.Lease.Token, Status: projectcoord.UsageRequestInterrupted})
	if err != nil {
		t.Fatal(err)
	}
	return coordinatorUsageFixture{store: store, workspace: workspace, home: home, run: run, record: record}
}

func (f coordinatorUsageFixture) receipt() projectcoord.UsageReceipt {
	actual := 0.12
	return projectcoord.UsageReceipt{ReceiptID: "account-receipt-1", EvidenceSHA256: strings.Repeat("c", 64), Authority: "operator-provider-export", Provider: f.record.Provider, EndpointSHA256: f.record.EndpointSHA256, Model: f.record.Model, ClientRequestID: f.record.ClientRequestID, Usage: projectcoord.TokenUsage{Input: 100, Output: 25}, ActualUSD: &actual}
}

func TestCoordinatorUsageMetadataNeverContainsClaimCredential(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	run, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	listing := coordinatorUsageList(run)
	data, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), f.record.ClaimToken) || strings.Contains(string(data), "claim_token") || strings.Contains(string(data), "receipts") {
		t.Fatalf("private ledger data leaked: %s", data)
	}
	if listing.Unresolved != 1 || listing.ActualCostUnknownRequests != 1 || listing.ActualKnownUSD != 0 || listing.EstimatedUSD != 0 {
		t.Fatalf("invented unresolved usage: %+v", listing)
	}
}

func TestCoordinatorUsageReceiptJSONStrictness(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	receipt, err := json.Marshal(f.receipt())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range [][]byte{receipt, append(append([]byte("["), receipt...), ']'), []byte("[]")} {
		if _, err := coordinatorUsageDecodeReceipts(value); err != nil {
			t.Fatal(err)
		}
	}
	invalid := []string{"null", "[null]", "{}", string(receipt) + " {}", strings.Replace(string(receipt), `"usage":{`, `"private_key":"hidden-secret","usage":{`, 1), strings.Replace(string(receipt), `"input":100`, `"input":100,"input":200`, 1), strings.Replace(string(receipt), `"cache_read":0,`, "", 1), strings.Replace(string(receipt), `"output":25`, `"output":null`, 1), strings.Repeat(" ", coordinatorUsageReceiptLimit+1)}
	invalid = append(invalid, strings.Replace(string(receipt), `,"actual_usd":`, `,"Usage":{"input":0,"output":0,"cache_read":0,"cache_write":0},"actual_usd":`, 1), strings.Replace(string(receipt), `"input":100`, `"input":100,"INPUT":0`, 1))
	for _, value := range invalid {
		if _, err := coordinatorUsageDecodeReceipts([]byte(value)); err == nil {
			t.Fatalf("accepted malformed receipt")
		} else if strings.Contains(err.Error(), "hidden-secret") {
			t.Fatal("decoder leaked raw private input")
		}
	}
}

func TestCoordinatorUsagePrivateMissingPreservesSafeNotExistSentinel(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	_, _, err := coordinatorUsageReadPrivateFile(filepath.Join(f.home, "missing.json"), f.workspace, coordinatorUsageReceiptLimit, false)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-file identity was lost: %v", err)
	}
}

func TestCoordinatorUsagePrivateInputBoundaries(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	path := filepath.Join(f.home, "receipt.json")
	if err := os.WriteFile(path, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinatorUsageReadPrivateFile(path, f.workspace, coordinatorUsageReceiptLimit, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinatorUsageReadPrivateFile(path, f.workspace, coordinatorUsageReceiptLimit, false); err == nil {
		t.Fatal("accepted public receipt")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.home, "linked.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinatorUsageReadPrivateFile(link, f.workspace, coordinatorUsageReceiptLimit, false); err == nil {
		t.Fatal("accepted receipt symlink")
	}
	inside := filepath.Join(f.workspace, "receipt.json")
	if err := os.WriteFile(inside, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordinatorUsageReadPrivateFile(inside, f.workspace, coordinatorUsageReceiptLimit, false); err == nil {
		t.Fatal("accepted workspace-controlled receipt")
	}
	if _, _, err := coordinatorUsageReadPrivateFile("relative.json", f.workspace, coordinatorUsageReceiptLimit, false); err == nil {
		t.Fatal("accepted relative receipt path")
	}
	if _, err := coordinatorUsageImport(context.Background(), f.store, f.workspace, f.run.ID, []projectcoord.UsageReceipt{f.receipt()}); err != nil {
		t.Fatal(err)
	}
	conflict := f.receipt()
	conflict.Usage.Input++
	if _, err := coordinatorUsageImport(context.Background(), f.store, f.workspace, f.run.ID, []projectcoord.UsageReceipt{conflict}); err == nil || !strings.Contains(err.Error(), "receipt_conflict") {
		t.Fatalf("reused receipt conflict was hidden: %v", err)
	}
}
