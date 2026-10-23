package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCoordinatorUsageWindowsNativeRecoveryFailsClosedWithoutIO(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	t.Setenv("METIS_HOME", f.home)
	before, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("host-private-billing-credential")
	privateFile := filepath.Join(f.home, "private-receipt.json")
	if err := os.WriteFile(privateFile, secret, 0600); err != nil {
		t.Fatal(err)
	}
	receiptDir := filepath.Join(f.home, "must-not-create", "receipts")
	assertUnsupported := func(err error) {
		t.Helper()
		var safe *coordinatorUsageError
		if !errors.As(err, &safe) || safe.code != "unsupported_platform_windows" {
			t.Fatalf("want explicit unsupported platform, got %v", err)
		}
		if strings.Contains(err.Error(), string(secret)) || strings.Contains(err.Error(), f.home) {
			t.Fatal("unsupported platform error exposed private input")
		}
	}

	// Reject before validating the store or creating the inbox. Invalid inputs
	// must not turn into file operations merely because Windows lacks POSIX ACLs.
	r, err := newCoordinatorUsageReconciler(nil, f.workspace, f.run.ID, f.home, receiptDir, privateFile, nil)
	assertUnsupported(err)
	if r != nil {
		t.Fatal("unsupported native recovery returned a usable reconciler")
	}
	r, err = newCoordinatorUsageReconciler(f.store, f.workspace, f.run.ID, f.home, receiptDir, privateFile, nil)
	assertUnsupported(err)
	if r != nil {
		t.Fatal("unsupported native recovery returned a usable reconciler")
	}
	if data, info, err := coordinatorUsageReadPrivateFile(privateFile, f.workspace, coordinatorUsageReceiptLimit, false); data != nil || info != nil {
		t.Fatal("unsupported platform exposed private file contents or metadata")
	} else {
		assertUnsupported(err)
	}
	if _, err := coordinatorUsagePrivateDirectory(f.home, nil); err == nil {
		t.Fatal("Windows directory was accepted without ACL validation")
	} else {
		assertUnsupported(err)
	}
	assertUnsupported(coordinatorUsageWriteEvidence(filepath.Join(f.home, "must-not-write.json"), secret, f.workspace))
	assertUnsupported(cmdCoordinatorUsageAdapterHelper(context.Background(), nil))
	assertUnsupported(cmdProjectCoordinatorUsage(context.Background(), []string{"import", f.run.ID, "--cwd", f.workspace, "--receipt-file", privateFile}))
	assertUnsupported(cmdProjectCoordinatorUsage(context.Background(), []string{"reconcile", f.run.ID, "--cwd", f.workspace, "--receipt-dir", receiptDir}))

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	config := coordinatorBillingWriteConfig(t, f, coordinatorBillingConfig{Provider: "openrouter", LedgerProvider: "selected-provider", InferenceEndpoint: server.URL + "/chat/completions", APIBaseURL: server.URL, TokenFile: privateFile})
	manual := &coordinatorUsageReconciler{workspace: f.workspace, receiptDir: receiptDir}
	assertUnsupported(manual.configureBilling(config))
	if calls != 0 || manual.billing != nil {
		t.Fatal("unsupported billing recovery reached the network or configured a source")
	}
	for _, path := range []string{filepath.Dir(receiptDir), filepath.Join(f.home, "must-not-write.json")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("unsupported recovery created %s: %v", path, err)
		}
	}
	if current, err := os.ReadFile(privateFile); err != nil || !reflect.DeepEqual(current, secret) {
		t.Fatal("unsupported recovery changed private file")
	}
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("unsupported recovery changed the usage ledger")
	}
	// Listing and strict JSON validation remain usable without host-private I/O.
	listing := coordinatorUsageList(after)
	data, err := json.Marshal(f.receipt())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinatorUsageDecodeReceipts(data); err != nil || listing.Unresolved != 1 || listing.ActualKnownUSD != 0 {
		t.Fatal("Windows native boundary disabled cross-platform ledger handling")
	}
}
