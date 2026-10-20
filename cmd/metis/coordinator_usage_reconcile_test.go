package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

func newCoordinatorUsageTestReconciler(store *projectcoord.Store, workspace, runID, home, receiptDir, command string, log func(coordinatorUsageReconcileEvent)) (*coordinatorUsageReconciler, error) {
	r, err := newCoordinatorUsageReconciler(store, workspace, runID, home, receiptDir, command, log)
	if err == nil {
		r.helperArguments = []string{"-test.run=^TestCoordinatorUsageAdapterHelperProcess$", "--"}
	}
	return r, err
}

func TestCoordinatorUsageAdapterHelperProcess(t *testing.T) {
	if os.Getenv("METIS_COORDINATOR_USAGE_HELPER") != "1" {
		return
	}
	separator := -1
	for i, argument := range os.Args {
		if argument == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		os.Exit(48)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := cmdCoordinatorUsageAdapterHelper(ctx, os.Args[separator+1:]); err != nil {
		os.Exit(49)
	}
	os.Exit(0)
}

func coordinatorUsageWriteReceipt(t *testing.T, path string, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func coordinatorUsageShellLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func coordinatorUsageAdapter(t *testing.T, f coordinatorUsageFixture, script string) string {
	t.Helper()
	path := filepath.Join(f.home, "trusted-adapter")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCoordinatorUsageInboxPersistsReceiptsAndReplaysWithoutDoubleCharging(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	r, err := newCoordinatorUsageTestReconciler(f.store, f.workspace, f.run.ID, f.home, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	data := coordinatorUsageWriteReceipt(t, filepath.Join(r.receiptDir, "received.json"), f.receipt())
	summary, err := r.CheckOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Imported != 1 || summary.Unresolved != 0 || summary.ActualKnownUSD != 0.12 || summary.ActualCostUnknownRequests != 0 {
		t.Fatalf("receipt reconciliation failed: %+v", summary)
	}
	entries, err := os.ReadDir(r.processedDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("lost processed evidence: %v %v", entries, err)
	}
	processed, err := os.ReadFile(filepath.Join(r.processedDir, entries[0].Name()))
	if err != nil || !bytes.Equal(processed, data) {
		t.Fatal("processed evidence changed")
	}
	if _, err := os.Stat(filepath.Join(r.receiptDir, "received.json")); !os.IsNotExist(err) {
		t.Fatal("receipt was not archived")
	}
	for i := 0; i < 2; i++ {
		coordinatorUsageWriteReceipt(t, filepath.Join(r.receiptDir, "replayed.json"), f.receipt())
		if _, err := r.CheckOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	run, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.SpentUSD != 0.12 || len(run.UsageRequests[0].Receipts) != 1 {
		t.Fatalf("receipt replay was charged twice: %+v", coordinatorUsageList(run))
	}
}

func TestCoordinatorUsageInboxScopeFailureRetainsEvidenceAndDoesNotBlockValidFile(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	r, err := newCoordinatorUsageTestReconciler(f.store, f.workspace, f.run.ID, f.home, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	wrong := f.receipt()
	wrong.Provider = "different-account-provider"
	badPath := filepath.Join(r.receiptDir, "bad.json")
	coordinatorUsageWriteReceipt(t, badPath, wrong)
	coordinatorUsageWriteReceipt(t, filepath.Join(r.receiptDir, "good.json"), f.receipt())
	summary, err := r.CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "receipt_scope_mismatch") || summary.FailedFiles != 1 || summary.Imported != 1 || summary.Unresolved != 0 {
		t.Fatalf("scope mismatch not reported: %+v %v", summary, err)
	}
	if _, err := os.Stat(badPath); err != nil {
		t.Fatal("lost failed evidence")
	}
}

func TestCoordinatorUsageAdapterUsesOnlyNonsecretIdentityAndAuthoritativeCost(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	t.Setenv("UPSTREAM_API_KEY", "private-provider-key-never-forward")
	receiptPath := filepath.Join(f.home, "authoritative-export.json")
	coordinatorUsageWriteReceipt(t, receiptPath, f.receipt())
	stdinPath, envPath := filepath.Join(f.home, "adapter-stdin"), filepath.Join(f.home, "adapter-env")
	command := coordinatorUsageAdapter(t, f, "/bin/cat > "+coordinatorUsageShellLiteral(stdinPath)+"\n/usr/bin/env > "+coordinatorUsageShellLiteral(envPath)+"\n/bin/cat "+coordinatorUsageShellLiteral(receiptPath))
	r, err := newCoordinatorUsageTestReconciler(f.store, f.workspace, f.run.ID, f.home, "", command, nil)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := r.CheckOnce(context.Background())
	if err != nil || summary.Imported != 1 || summary.ActualKnownUSD != 0.12 {
		t.Fatalf("adapter result: %+v %v", summary, err)
	}
	stdin, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stdin, []byte(f.record.ClientRequestID)) || !bytes.Contains(stdin, []byte(f.record.EndpointSHA256)) || bytes.Contains(stdin, []byte("claim_token")) || bytes.Contains(stdin, []byte(f.record.ClaimToken)) {
		t.Fatalf("adapter identity whitelist failed: %s", stdin)
	}
	env, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(env, []byte("private-provider-key-never-forward")) || bytes.Contains(env, []byte("UPSTREAM_API_KEY")) {
		t.Fatal("adapter inherited provider authority")
	}
}

func TestCoordinatorUsageAdapterEmptyResultKeepsUsageUnknown(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	command := coordinatorUsageAdapter(t, f, "/bin/cat > /dev/null\nprintf '[]'")
	r, err := newCoordinatorUsageTestReconciler(f.store, f.workspace, f.run.ID, f.home, "", command, nil)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := r.CheckOnce(context.Background())
	if err != nil || summary.Imported != 0 || summary.Unresolved != 1 || summary.ActualCostUnknownRequests != 1 || summary.ActualKnownUSD != 0 {
		t.Fatalf("empty adapter invented charge: %+v %v", summary, err)
	}
	run, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil || run.SpentUSD != 0 || run.UsageRequests[0].UsageReported {
		t.Fatalf("unknown usage was overwritten: %v", err)
	}
}

func TestCoordinatorUsageAdapterCancelAfterReadyKillsOwnedDescendant(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	pidPath := filepath.Join(f.home, "child-pid")
	command := coordinatorUsageAdapter(t, f, "/bin/sleep 60 &\nprintf '%s' \"$!\" > "+coordinatorUsageShellLiteral(pidPath)+"\nwait")
	r, err := newCoordinatorUsageTestReconciler(f.store, f.workspace, f.run.ID, f.home, "", command, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := r.CheckOnce(ctx); done <- err }()
	var pidBytes []byte
	readyDeadline := time.Now().Add(5 * time.Second)
	for {
		pidBytes, err = os.ReadFile(pidPath)
		if err == nil && len(pidBytes) > 0 {
			break
		}
		if time.Now().After(readyDeadline) {
			cancel()
			<-done
			t.Fatal("adapter descendant did not reach ready barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatal(err)
	}
	// Cancellation starts only after a real running child has identified itself.
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("adapter cancellation was hidden: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("adapter helper did not join after cancellation")
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := coordinatorProcessBirthIdentity(pid); err != nil {
			break
		}
		if time.Now().After(deadline) {
			if child, err := os.FindProcess(pid); err == nil {
				_ = child.Kill()
			}
			t.Fatal("adapter descendant survived timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCoordinatorUsageAdapterTimeoutIsBounded(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	command := coordinatorUsageAdapter(t, f, "/bin/sleep 60")
	r, err := newCoordinatorUsageTestReconciler(f.store, f.workspace, f.run.ID, f.home, "", command, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.commandTimeout = time.Second
	started := time.Now()
	_, err = r.CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "adapter_timeout") || time.Since(started) > 4*time.Second {
		t.Fatalf("adapter timeout is not bounded: %v", err)
	}
}

func TestCoordinatorUsageAtomicEvidencePublicationAndConcurrentReconcile(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	r, err := newCoordinatorUsageTestReconciler(f.store, f.workspace, f.run.ID, f.home, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.receiptDir, "concurrent.json")
	data, err := json.Marshal(f.receipt())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errorsSeen <- coordinatorUsageWriteEvidence(path, data, f.workspace) }()
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	other, err := newCoordinatorUsageTestReconciler(f.store, f.workspace, f.run.ID, f.home, r.receiptDir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, reconciler := range []*coordinatorUsageReconciler{r, other} {
		wg.Add(1)
		go func(r *coordinatorUsageReconciler) {
			defer wg.Done()
			_, err := r.CheckOnce(context.Background())
			results <- err
		}(reconciler)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	run, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil || run.SpentUSD != 0.12 || len(run.UsageRequests[0].Receipts) != 1 {
		t.Fatalf("concurrent import double charged: %v", err)
	}
}

func TestCoordinatorUsageAdapterCallerHelperProcess(t *testing.T) {
	if os.Getenv("METIS_USAGE_CALLER_TEST") != "1" {
		return
	}
	var options struct{ StoreRoot, Workspace, Home, RunID, Command string }
	data, err := os.ReadFile(os.Getenv("METIS_USAGE_CALLER_OPTIONS"))
	if err != nil || json.Unmarshal(data, &options) != nil {
		os.Exit(50)
	}
	r, err := newCoordinatorUsageTestReconciler(projectcoord.NewStore(options.StoreRoot), options.Workspace, options.RunID, options.Home, "", options.Command, nil)
	if err != nil {
		os.Exit(51)
	}
	_, _ = r.CheckOnce(context.Background())
	os.Exit(0)
}

func TestCoordinatorUsageAdapterHelperSurvivesCallerSIGKILLAndStopsAdapter(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	shellPath, childPath, helperPath := filepath.Join(f.home, "shell-pid"), filepath.Join(f.home, "sleep-pid"), filepath.Join(f.home, "helper-pid")
	command := coordinatorUsageAdapter(t, f, "printf '%s' \"$$\" > "+coordinatorUsageShellLiteral(shellPath)+"\nprintf '%s' \"$PPID\" > "+coordinatorUsageShellLiteral(helperPath)+"\n/bin/sleep 60 &\nprintf '%s' \"$!\" > "+coordinatorUsageShellLiteral(childPath)+"\nwait")
	optionsPath := filepath.Join(f.home, "caller-options.json")
	coordinatorUsageWriteReceipt(t, optionsPath, struct{ StoreRoot, Workspace, Home, RunID, Command string }{f.store.Root(), f.workspace, f.home, f.run.ID, command})
	caller := exec.Command(os.Args[0], "-test.run=^TestCoordinatorUsageAdapterCallerHelperProcess$")
	caller.Env = append(os.Environ(), "METIS_USAGE_CALLER_TEST=1", "METIS_USAGE_CALLER_OPTIONS="+optionsPath)
	if err := caller.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = caller.Process.Kill(); _ = caller.Wait() }()
	pids := map[string]int{}
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, path := range []string{shellPath, childPath, helperPath} {
			if data, err := os.ReadFile(path); err == nil {
				if pid, err := strconv.Atoi(string(data)); err == nil {
					pids[path] = pid
				}
			}
		}
		if len(pids) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owned adapter did not reach PID ready barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	births := map[int]string{}
	for _, pid := range pids {
		identity, err := coordinatorProcessBirthIdentity(pid)
		if err != nil {
			t.Fatal(err)
		}
		births[pid] = identity
	}
	defer func() {
		for pid, birth := range births {
			if current, err := coordinatorProcessBirthIdentity(pid); err == nil && current == birth {
				if process, err := os.FindProcess(pid); err == nil {
					if pid == pids[shellPath] {
						jobs.KillProcessGroup(process)
					} else {
						_ = process.Kill()
					}
				}
			}
		}
	}()
	if err := caller.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = caller.Wait()
	deadline = time.Now().Add(4 * time.Second)
	for {
		alive := false
		for _, pid := range pids {
			if _, err := coordinatorProcessBirthIdentity(pid); err == nil {
				alive = true
			}
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("adapter/helper survived caller SIGKILL")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCoordinatorUsageAdapterRejectsOverflowRawSecretsAndChangedExecutable(t *testing.T) {
	for _, mode := range []string{"overflow", "private", "changed"} {
		t.Run(mode, func(t *testing.T) {
			f := newCoordinatorUsageFixture(t)
			script := "printf '[]'"
			if mode == "overflow" {
				script = "/usr/bin/yes"
			}
			if mode == "private" {
				script = "printf 'provider-private-raw-secret'; printf 'stderr-private-secret' >&2"
			}
			command := coordinatorUsageAdapter(t, f, script)
			r, err := newCoordinatorUsageTestReconciler(f.store, f.workspace, f.run.ID, f.home, "", command, nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "changed" {
				if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '[]'\n# changed\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			_, err = r.CheckOnce(context.Background())
			if err == nil || strings.Contains(err.Error(), "private-raw-secret") || strings.Contains(err.Error(), "stderr-private-secret") {
				t.Fatalf("adapter failure was accepted/leaked: %v", err)
			}
		})
	}
}

func TestCoordinatorUsageRunChecksAtStartupAndJoinsOnCancel(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	events := make(chan coordinatorUsageReconcileEvent, 4)
	r, err := newCoordinatorUsageTestReconciler(f.store, f.workspace, f.run.ID, f.home, "", "", func(e coordinatorUsageReconcileEvent) { events <- e })
	if err != nil {
		t.Fatal(err)
	}
	coordinatorUsageWriteReceipt(t, filepath.Join(r.receiptDir, "startup.json"), f.receipt())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	select {
	case event := <-events:
		if event.Imported != 1 || event.ErrorCode != "" {
			t.Fatalf("bad startup event: %+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconciler waited for ticker before its first check")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconciler did not join on cancel")
	}
}

func TestCoordinatorUsageTrustPathsAndLiveRequestFiltering(t *testing.T) {
	f := newCoordinatorUsageFixture(t)
	if _, err := newCoordinatorUsageTestReconciler(f.store, f.workspace, f.run.ID, f.home, filepath.Join(f.workspace, "receipts"), "", nil); err == nil {
		t.Fatal("accepted workspace receipt inbox")
	}
	command := filepath.Join(f.workspace, "agent-controlled-script")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '[]'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := newCoordinatorUsageTestReconciler(f.store, f.workspace, f.run.ID, f.home, "", command, nil); err == nil {
		t.Fatal("accepted workspace adapter")
	}
	link := filepath.Join(f.home, "receipt-link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := newCoordinatorUsageTestReconciler(f.store, f.workspace, f.run.ID, f.home, link, "", nil); err == nil {
		t.Fatal("accepted inbox symlink")
	}
	run, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	run.UsageRequests[0].Status = "started"
	if got := coordinatorUsageEligibleRequests(run, time.Now()); len(got) != 0 {
		t.Fatal("adapter queried still-live request")
	}
	run.Items[0].Lease.ExpiresAt = time.Now().Add(-time.Second)
	if got := coordinatorUsageEligibleRequests(run, time.Now()); len(got) != 1 {
		t.Fatal("orphan request was hidden")
	}
}
