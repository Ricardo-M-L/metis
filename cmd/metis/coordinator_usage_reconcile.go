package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/processutil"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

type coordinatorUsageReconcileSummary struct {
	RunID                     string  `json:"run_id"`
	FilesProcessed            int     `json:"files_processed"`
	Imported                  int     `json:"imported"`
	FailedFiles               int     `json:"failed_files"`
	Unresolved                int     `json:"unresolved"`
	ActualKnownUSD            float64 `json:"actual_known_usd"`
	ActualCostUnknownRequests int     `json:"actual_cost_unknown_requests"`
}

type coordinatorUsageReconcileEvent struct {
	At         time.Time `json:"at"`
	Kind       string    `json:"kind"`
	RunID      string    `json:"run_id"`
	Imported   int       `json:"imported"`
	Unresolved int       `json:"unresolved"`
	ErrorCode  string    `json:"error_code,omitempty"`
}

type coordinatorUsageReconciler struct {
	helperExecutable                                          string
	helperArguments                                           []string
	mu                                                        sync.Mutex
	store                                                     *projectcoord.Store
	workspace, runID, home, receiptDir, processedDir, command string
	directoryInfo, processedInfo                              os.FileInfo
	commandSHA256                                             [32]byte
	interval, commandTimeout                                  time.Duration
	log                                                       func(coordinatorUsageReconcileEvent)
	billing                                                   *coordinatorBillingReconciler
}

func newCoordinatorUsageReconciler(store *projectcoord.Store, workspace, runID, home, receiptDir, command string, log func(coordinatorUsageReconcileEvent)) (*coordinatorUsageReconciler, error) {
	if store == nil {
		return nil, coordinatorUsageFailure("missing_store")
	}
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil || !filepath.IsAbs(workspace) {
		return nil, coordinatorUsageFailure("invalid_workspace")
	}
	// Validation precedes path creation: a malicious run ID never becomes a
	// filesystem component merely because a user supplied it on the command.
	run, err := store.Get(workspace, runID)
	if err != nil {
		return nil, err
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return nil, coordinatorUsageFailure("invalid_home")
	}
	if receiptDir == "" {
		receiptDir = filepath.Join(home, "coordinator-usage-receipts", run.ID)
	}
	if !filepath.IsAbs(receiptDir) {
		return nil, coordinatorUsageFailure("receipt_directory_requires_absolute_path")
	}
	receiptDir, err = coordinatorUsageCanonicalFuturePath(receiptDir)
	if err != nil || !coordinatorUsageOutsideWorkspace(receiptDir, workspace) {
		return nil, coordinatorUsageFailure("receipt_directory_must_be_outside_workspace")
	}
	if err := os.MkdirAll(receiptDir, 0700); err != nil {
		return nil, coordinatorUsageFailure("receipt_directory_create_failed")
	}
	directoryInfo, err := coordinatorUsagePrivateDirectory(receiptDir, nil)
	if err != nil {
		return nil, err
	}
	processed := filepath.Join(receiptDir, "processed")
	if err := os.MkdirAll(processed, 0700); err != nil {
		return nil, coordinatorUsageFailure("receipt_directory_create_failed")
	}
	processedInfo, err := coordinatorUsagePrivateDirectory(processed, nil)
	if err != nil {
		return nil, err
	}
	r := &coordinatorUsageReconciler{store: store, workspace: workspace, runID: run.ID, home: home, receiptDir: receiptDir, processedDir: processed, directoryInfo: directoryInfo, processedInfo: processedInfo, interval: 30 * time.Second, commandTimeout: 20 * time.Second, log: log}
	r.helperExecutable, err = os.Executable()
	if err != nil {
		return nil, coordinatorUsageFailure("helper_executable_unavailable")
	}
	r.helperArguments = []string{"coordinator", "usage", "adapter-helper"}
	if command != "" {
		data, _, err := coordinatorUsageReadPrivateFile(command, workspace, 128<<20, true)
		if err != nil {
			return nil, err
		}
		r.command, err = filepath.EvalSymlinks(command)
		if err != nil {
			return nil, coordinatorUsageFailure("invalid_adapter_path")
		}
		r.commandSHA256 = sha256.Sum256(data)
	}
	return r, nil
}

func coordinatorUsageCanonicalFuturePath(path string) (string, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", coordinatorUsageFailure("receipt_directory_symlink")
	}
	var suffix []string
	current := filepath.Clean(path)
	for {
		if _, err := os.Lstat(current); err == nil {
			real, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				real = filepath.Join(real, suffix[i])
			}
			return real, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", coordinatorUsageFailure("invalid_receipt_directory")
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func coordinatorUsagePrivateDirectory(path string, expected os.FileInfo) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !coordinatorUsageOwned(info) || (expected != nil && !os.SameFile(expected, info)) {
		return nil, coordinatorUsageFailure("unsafe_receipt_directory")
	}
	return info, nil
}

// The caller owns a goroutine and joins Run after cancelling its context.
// CheckOnce is serialized, so an interval never overlaps a slow adapter.
func (r *coordinatorUsageReconciler) Run(ctx context.Context) error {
	check := func() {
		summary, err := r.CheckOnce(ctx)
		if r.log == nil || (summary.Imported == 0 && err == nil) {
			return
		}
		event := coordinatorUsageReconcileEvent{At: time.Now().UTC(), Kind: "usage_reconciliation", RunID: r.runID, Imported: summary.Imported, Unresolved: summary.Unresolved}
		if err != nil {
			event.ErrorCode = "reconciliation_failed"
			var safe *coordinatorUsageError
			if errors.As(err, &safe) {
				event.ErrorCode = safe.code
			} else if ctx.Err() != nil {
				event.ErrorCode = "reconciliation_cancelled"
			}
		}
		r.log(event)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	check()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			check()
		}
	}
}

func (r *coordinatorUsageReconciler) CheckOnce(ctx context.Context) (coordinatorUsageReconcileSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	summary := coordinatorUsageReconcileSummary{RunID: r.runID}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	if _, err := coordinatorUsagePrivateDirectory(r.receiptDir, r.directoryInfo); err != nil {
		return summary, err
	}
	if _, err := coordinatorUsagePrivateDirectory(r.processedDir, r.processedInfo); err != nil {
		return summary, err
	}
	entries, err := os.ReadDir(r.receiptDir)
	if err != nil {
		return summary, coordinatorUsageFailure("receipt_directory_read_failed")
	}
	if len(entries) > 1024 {
		return summary, coordinatorUsageFailure("receipt_directory_entry_limit")
	}
	var firstError error
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		imported, err := r.importFile(ctx, filepath.Join(r.receiptDir, entry.Name()))
		summary.Imported += imported
		if err != nil {
			summary.FailedFiles++
			if firstError == nil {
				firstError = err
			}
			continue
		}
		summary.FilesProcessed++
	}
	run, err := r.store.Get(r.workspace, r.runID)
	if err != nil {
		return summary, coordinatorUsageFailure("usage_store_read_failed")
	}
	requests := coordinatorUsageEligibleRequests(run, time.Now())
	if r.command != "" && len(requests) > 0 {
		receiptData, err := r.queryAdapter(ctx, requests)
		if err == nil {
			var receipts []projectcoord.UsageReceipt
			receipts, err = coordinatorUsageDecodeReceipts(receiptData)
			if err == nil && len(receipts) > 0 {
				// Keep returned evidence on disk before importing. A service crash
				// after a partial import leaves replayable, idempotent receipts.
				sum := sha256.Sum256(receiptData)
				path := filepath.Join(r.receiptDir, "adapter-"+hex.EncodeToString(sum[:])+".json")
				err = coordinatorUsageWriteEvidence(path, receiptData, r.workspace)
				if err == nil {
					var imported int
					imported, err = r.importFile(ctx, path)
					summary.Imported += imported
					if err == nil {
						summary.FilesProcessed++
					}
				}
			}
		}
		if err != nil {
			summary.FailedFiles++
			if firstError == nil {
				firstError = err
			}
		}
	}
	run, err = r.store.Get(r.workspace, r.runID)
	if err == nil && r.billing != nil {
		count, billingErr := r.reconcileBilling(ctx, run)
		summary.Imported += count
		if billingErr != nil {
			summary.FailedFiles++
			if firstError == nil {
				firstError = billingErr
			}
		}
		run, err = r.store.Get(r.workspace, r.runID)
	}
	if err != nil {
		return summary, coordinatorUsageFailure("usage_store_read_failed")
	}
	if projectionErr := coordinatorBillingProjectJobUsage(run); projectionErr != nil && firstError == nil {
		firstError = projectionErr
	}
	listing := coordinatorUsageList(run)
	summary.Unresolved, summary.ActualKnownUSD, summary.ActualCostUnknownRequests = listing.Unresolved, listing.ActualKnownUSD, listing.ActualCostUnknownRequests
	return summary, firstError
}

func coordinatorUsageEligibleRequests(run projectcoord.Run, now time.Time) []coordinatorUsageMetadata {
	result := []coordinatorUsageMetadata{}
	for _, request := range projectcoord.UnresolvedUsageRequests(run) {
		active := false
		if request.Status == projectcoord.UsageRequestStarted {
			for _, item := range run.Items {
				if item.ID == request.ItemID && item.Status == projectcoord.ItemRunning && item.Lease != nil && item.Owner == request.Worker && item.Lease.Token == request.ClaimToken && item.Lease.ExpiresAt.After(now) {
					active = true
					break
				}
			}
		}
		if !active {
			result = append(result, coordinatorUsagePublic(request))
		}
	}
	return result
}

func coordinatorUsageWriteEvidence(path string, data []byte, workspace string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".usage-evidence-*")
	if err != nil {
		return coordinatorUsageFailure("receipt_write_failed")
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return coordinatorUsageFailure("receipt_write_failed")
	}
	if err := file.Sync(); err != nil {
		return coordinatorUsageFailure("receipt_write_failed")
	}
	if err := file.Close(); err != nil {
		return coordinatorUsageFailure("receipt_write_failed")
	}
	// Publish a fully synced inode without replacing another writer's receipt.
	for attempts := 0; attempts < 3; attempts++ {
		err := os.Link(file.Name(), path)
		if err == nil {
			return nil
		}
		if !os.IsExist(err) {
			return coordinatorUsageFailure("receipt_write_failed")
		}
		previous, _, err := coordinatorUsageReadPrivateFile(path, workspace, coordinatorUsageReceiptLimit, false)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !bytes.Equal(previous, data) {
			return coordinatorUsageFailure("receipt_file_conflict")
		}
		return nil
	}
	return coordinatorUsageFailure("receipt_write_failed")
}

func (r *coordinatorUsageReconciler) importFile(ctx context.Context, path string) (int, error) {
	data, before, err := coordinatorUsageReadPrivateFile(path, r.workspace, coordinatorUsageReceiptLimit, false)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	receipts, err := coordinatorUsageDecodeReceipts(data)
	if err != nil {
		return 0, err
	}
	imported, err := coordinatorUsageImport(ctx, r.store, r.workspace, r.runID, receipts)
	if err != nil {
		return imported, err
	}
	sum := sha256.Sum256(data)
	processedPath := filepath.Join(r.processedDir, hex.EncodeToString(sum[:])+".json")
	if err := coordinatorUsageWriteEvidence(processedPath, data, r.workspace); err != nil {
		return imported, err
	}
	current, observed, err := coordinatorUsageReadPrivateFile(path, r.workspace, coordinatorUsageReceiptLimit, false)
	if errors.Is(err, os.ErrNotExist) {
		return imported, nil
	}
	if err != nil || !os.SameFile(before, observed) || !bytes.Equal(current, data) {
		return imported, coordinatorUsageFailure("receipt_file_changed")
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return imported, coordinatorUsageFailure("receipt_archive_failed")
	}
	return imported, nil
}

type coordinatorUsageBoundedOutput struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
	cancel   context.CancelFunc
}

func (w *coordinatorUsageBoundedOutput) Write(data []byte) (int, error) {
	if len(data) > w.limit-w.buffer.Len() {
		w.overflow = true
		w.cancel()
		return 0, coordinatorUsageFailure("adapter_output_limit")
	}
	return w.buffer.Write(data)
}

func (r *coordinatorUsageReconciler) queryAdapter(parent context.Context, requests []coordinatorUsageMetadata) ([]byte, error) {
	data, _, err := coordinatorUsageReadPrivateFile(r.command, r.workspace, 128<<20, true)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(data) != r.commandSHA256 {
		return nil, coordinatorUsageFailure("adapter_executable_changed")
	}
	input, err := json.Marshal(struct {
		Schema   int                        `json:"schema"`
		RunID    string                     `json:"run_id"`
		Requests []coordinatorUsageMetadata `json:"requests"`
	}{1, r.runID, requests})
	if err != nil {
		return nil, coordinatorUsageFailure("adapter_input_failed")
	}
	ctx, cancel := context.WithTimeout(parent, r.commandTimeout)
	defer cancel()
	arguments := append(append([]string(nil), r.helperArguments...), "--command", r.command, "--cwd", r.workspace, "--home", r.home, "--expected-sha256", hex.EncodeToString(r.commandSHA256[:]), "--timeout", r.commandTimeout.String())
	command := exec.CommandContext(ctx, r.helperExecutable, arguments...)
	command.Dir = r.home
	userHome, err := os.UserHomeDir()
	if err != nil {
		return nil, coordinatorUsageFailure("adapter_home_failed")
	}
	command.Env = []string{"HOME=" + userHome, "METIS_HOME=" + r.home, "PATH=" + os.Getenv("PATH")}
	command.Env = append(command.Env, coordinatorParentEnvironment()...)
	command.Env = append(command.Env, "METIS_COORDINATOR_USAGE_HELPER=1")
	command.Stdin = bytes.NewReader(input)
	output := &coordinatorUsageBoundedOutput{limit: coordinatorUsageReceiptLimit, cancel: cancel}
	command.Stdout, command.Stderr = output, io.Discard
	jobs.ApplyProcessGroup(command)
	// Give the helper its SIGTERM cancellation path so it joins the actual
	// adapter group. Its own birth watcher independently handles caller SIGKILL.
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return processutil.Terminate(command.Process.Pid)
	}
	command.WaitDelay = 3 * time.Second
	err = command.Run()
	jobs.KillProcessGroup(command.Process)
	if output.overflow {
		return nil, coordinatorUsageFailure("adapter_output_limit")
	}
	if parent.Err() != nil {
		return nil, parent.Err()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, coordinatorUsageFailure("adapter_timeout")
	}
	if err != nil {
		return nil, coordinatorUsageFailure("adapter_failed")
	}
	return output.buffer.Bytes(), nil
}

// Private CLI mode. It performs no model calls; this independent helper keeps
// the adapter deadline and lifecycle ownership when the service is SIGKILLed.
func cmdCoordinatorUsageAdapterHelper(parent context.Context, args []string) error {
	if os.Getenv("METIS_COORDINATOR_USAGE_HELPER") != "1" || os.Getenv(coordinatorParentPIDEnv) == "" {
		return coordinatorUsageFailure("helper_requires_parent_guard")
	}
	fs := flag.NewFlagSet("coordinator usage adapter-helper", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	commandPath := fs.String("command", "", "")
	workspace := fs.String("cwd", "", "")
	home := fs.String("home", "", "")
	expectedSHA := fs.String("expected-sha256", "", "")
	timeout := fs.Duration("timeout", 20*time.Second, "")
	if fs.Parse(args) != nil || len(fs.Args()) != 0 || *timeout <= 0 || *timeout > 20*time.Second {
		return coordinatorUsageFailure("invalid_helper_arguments")
	}
	guarded, closeWatch, err := coordinatorChildContext(parent)
	if err != nil {
		return coordinatorUsageFailure("helper_parent_unavailable")
	}
	defer closeWatch()
	ctx, cancel := context.WithTimeout(guarded, *timeout)
	defer cancel()
	data, _, err := coordinatorUsageReadPrivateFile(*commandPath, *workspace, 128<<20, true)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != *expectedSHA {
		return coordinatorUsageFailure("adapter_executable_changed")
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, coordinatorUsageReceiptLimit+1))
	if err != nil || len(input) > coordinatorUsageReceiptLimit {
		return coordinatorUsageFailure("adapter_input_failed")
	}
	var identity struct {
		Schema   int                        `json:"schema"`
		RunID    string                     `json:"run_id"`
		Requests []coordinatorUsageMetadata `json:"requests"`
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&identity) != nil || identity.Schema != 1 || identity.RunID == "" || decoder.Decode(new(any)) != io.EOF {
		return coordinatorUsageFailure("adapter_input_failed")
	}
	command := exec.CommandContext(ctx, *commandPath)
	command.Dir = *home
	userHome, err := os.UserHomeDir()
	if err != nil {
		return coordinatorUsageFailure("adapter_home_failed")
	}
	command.Env = []string{"HOME=" + userHome, "METIS_HOME=" + *home, "PATH=" + os.Getenv("PATH")}
	command.Stdin = bytes.NewReader(input)
	output := &coordinatorUsageBoundedOutput{limit: coordinatorUsageReceiptLimit, cancel: cancel}
	command.Stdout, command.Stderr = output, io.Discard
	jobs.ApplyProcessGroup(command)
	command.Cancel = func() error { jobs.KillProcessGroup(command.Process); return nil }
	command.WaitDelay = time.Second
	err = command.Run()
	jobs.KillProcessGroup(command.Process)
	if output.overflow {
		return coordinatorUsageFailure("adapter_output_limit")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return coordinatorUsageFailure("adapter_failed")
	}
	if _, err := coordinatorUsageDecodeReceipts(output.buffer.Bytes()); err != nil {
		return err
	}
	_, err = os.Stdout.Write(output.buffer.Bytes())
	return err
}
