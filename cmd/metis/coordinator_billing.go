package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/billing"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

// This opt-in configuration belongs to the host. No credential or path is
// supplied to the model, included in public usage metadata, or stored in a run.
type coordinatorBillingConfig struct {
	Provider          string `json:"provider"`
	LedgerProvider    string `json:"ledger_provider"`
	InferenceEndpoint string `json:"inference_endpoint"`
	APIBaseURL        string `json:"api_base_url,omitempty"`
	TokenFile         string `json:"token_file,omitempty"`
	UsageDB           string `json:"usage_db,omitempty"`
	SQLiteExecutable  string `json:"sqlite_executable,omitempty"`
	CorrectionWindow  string `json:"correction_window,omitempty"`
}

type coordinatorBillingReconciler struct {
	source           billing.Source
	config           coordinatorBillingConfig
	correctionWindow time.Duration
	endpointSHA256   string
	next             map[string]time.Time
	evidenceDir      string
	directoryInfo    os.FileInfo
}

func (r *coordinatorUsageReconciler) configureBilling(path string) error {
	if path == "" {
		return nil
	}
	data, _, err := coordinatorUsageReadPrivateFile(path, r.workspace, 64<<10, false)
	if err != nil {
		return err
	}
	if coordinatorUsageUniqueJSON(data) != nil {
		return coordinatorUsageFailure("invalid_billing_configuration")
	}
	var config coordinatorBillingConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil || decoder.Decode(new(any)) != io.EOF {
		return coordinatorUsageFailure("invalid_billing_configuration")
	}
	window := 7 * 24 * time.Hour
	if config.CorrectionWindow != "" {
		window, err = time.ParseDuration(config.CorrectionWindow)
		if err != nil || window < 0 || window > 30*24*time.Hour {
			return coordinatorUsageFailure("invalid_billing_correction_window")
		}
	}
	var source billing.Source
	endpoint, err := billing.EndpointSHA256(config.InferenceEndpoint)
	if err != nil {
		return coordinatorUsageFailure("invalid_billing_source")
	}
	switch config.Provider {
	case "openrouter":
		if config.UsageDB != "" || config.SQLiteExecutable != "" {
			return coordinatorUsageFailure("invalid_billing_configuration")
		}
		token, _, err := coordinatorUsageReadPrivateFile(config.TokenFile, r.workspace, 16<<10, false)
		if err != nil {
			return err
		}
		source, err = billing.NewOpenRouter(billing.OpenRouterConfig{APIBaseURL: config.APIBaseURL, InferenceEndpoint: config.InferenceEndpoint, LedgerProvider: config.LedgerProvider, Token: strings.TrimSpace(string(token))})
		if err != nil {
			return coordinatorUsageFailure("invalid_billing_source")
		}
	case "cliproxy_gui":
		if config.APIBaseURL != "" || config.TokenFile != "" {
			return coordinatorUsageFailure("invalid_billing_configuration")
		}
		if config.SQLiteExecutable == "" {
			config.SQLiteExecutable = "/usr/bin/sqlite3"
		}
		for _, file := range []string{config.UsageDB, config.SQLiteExecutable} {
			if err := coordinatorBillingTrustedHostFile(file, r.workspace); err != nil {
				return err
			}
		}
		source, err = billing.NewCLIProxyGUI(billing.CLIProxyGUIConfig{Database: config.UsageDB, SQLiteExecutable: config.SQLiteExecutable, InferenceEndpoint: config.InferenceEndpoint, LedgerProvider: config.LedgerProvider})
		if err != nil {
			return coordinatorUsageFailure("invalid_billing_source")
		}
	default:
		return coordinatorUsageFailure("unsupported_billing_provider")
	}
	dir := filepath.Join(r.receiptDir, "provider-evidence")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return coordinatorUsageFailure("billing_evidence_create_failed")
	}
	info, err := coordinatorUsagePrivateDirectory(dir, nil)
	if err != nil {
		return err
	}
	r.billing = &coordinatorBillingReconciler{source: source, config: config, correctionWindow: window, endpointSHA256: endpoint, next: map[string]time.Time{}, evidenceDir: dir, directoryInfo: info}
	return nil
}

func coordinatorBillingTrustedHostFile(path, workspace string) error {
	if !filepath.IsAbs(path) {
		return coordinatorUsageFailure("billing_source_requires_absolute_path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return coordinatorUsageFailure("unsafe_billing_source_file")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil || !coordinatorUsageOutsideWorkspace(real, workspace) {
		return coordinatorUsageFailure("billing_source_must_be_outside_workspace")
	}
	return nil
}

// Project this exact run, including a run already archived out of the job's
// active list. Re-enumerating RefreshRuns would miss a late supplier correction
// to that archived assignment and leave its lifetime budget stale.
func coordinatorBillingProjectJobUsage(run projectcoord.Run) error {
	bindings := make(map[string]principal.Binding)
	for _, item := range run.Items {
		if item.PrincipalBinding != nil {
			bindings[item.PrincipalBinding.JobID] = *item.PrincipalBinding
		}
	}
	for _, binding := range bindings {
		projection, err := persistentJobRunUsage(run, binding)
		if err != nil {
			return coordinatorUsageFailure("billing_job_projection_failed")
		}
		if _, err := persistentJobStore().RecordRunUsage(binding, projection); err != nil {
			return coordinatorUsageFailure("billing_job_projection_failed")
		}
	}
	return nil
}

// run is a fresh snapshot after inbox imports. The run lock still verifies
// receipt supersession at import time, so concurrent reconcile processes cannot
// overwrite each other's newer evidence.
func (r *coordinatorUsageReconciler) reconcileBilling(parent context.Context, run projectcoord.Run) (int, error) {
	b := r.billing
	if b == nil {
		return 0, nil
	}
	if _, err := coordinatorUsagePrivateDirectory(b.evidenceDir, b.directoryInfo); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	now := time.Now()
	requests := append([]projectcoord.UsageRequestRecord(nil), run.UsageRequests...)
	// Unknown charges are examined before periodic correction checks. Sorting
	// by next check prevents a permanently missing bill starving later requests.
	sort.SliceStable(requests, func(i, j int) bool {
		if (requests[i].ActualUSD == nil) != (requests[j].ActualUSD == nil) {
			return requests[i].ActualUSD == nil
		}
		return b.next[requests[i].ClientRequestID].Before(b.next[requests[j].ClientRequestID])
	})
	imported, examined := 0, 0
	var firstError error
	for _, request := range requests {
		if request.Provider != b.config.LedgerProvider || request.EndpointSHA256 != b.endpointSHA256 || request.Status == projectcoord.UsageRequestRejected || now.Before(b.next[request.ClientRequestID]) {
			continue
		}
		if request.Status == projectcoord.UsageRequestStarted {
			active := false
			for _, item := range run.Items {
				if item.ID == request.ItemID && item.Status == projectcoord.ItemRunning && item.Lease != nil && item.Lease.ExpiresAt.After(now) && item.Lease.Token == request.ClaimToken {
					active = true
					break
				}
			}
			if active {
				continue
			}
		}
		if request.ActualUSD != nil && (b.correctionWindow == 0 || now.Sub(request.StartedAt) > b.correctionWindow) {
			continue
		}
		if b.config.Provider == "cliproxy_gui" && request.Status == projectcoord.UsageRequestReconciled {
			continue
		}
		if examined >= 64 {
			break
		}
		examined++
		b.next[request.ClientRequestID] = now.Add(time.Minute)
		if request.ActualUSD != nil {
			b.next[request.ClientRequestID] = now.Add(time.Hour)
		}
		if b.config.Provider == "cliproxy_gui" {
			if err := coordinatorBillingTrustedHostFile(b.config.UsageDB, r.workspace); err != nil {
				return imported, err
			}
			if err := coordinatorBillingTrustedHostFile(b.config.SQLiteExecutable, r.workspace); err != nil {
				return imported, err
			}
		}
		observation, err := b.source.Lookup(ctx, billing.Request{Provider: request.Provider, EndpointSHA256: request.EndpointSHA256, Model: request.Model, ClientRequestID: request.ClientRequestID, ProviderRequestID: request.ProviderRequestID, ResponseID: request.ResponseID})
		if errors.Is(err, billing.ErrPending) {
			continue
		}
		if err != nil {
			if firstError == nil {
				firstError = coordinatorUsageFailure("billing_lookup_failed")
			}
			if ctx.Err() != nil {
				break
			}
			continue
		}
		receipt := observation.Receipt
		replay := false
		for _, previous := range request.Receipts {
			if previous.ReceiptID == receipt.ReceiptID {
				replay = true
				break
			}
		}
		if replay {
			continue
		}
		if len(request.Receipts) > 0 {
			receipt.SupersedesReceiptID = request.Receipts[len(request.Receipts)-1].ReceiptID
		}
		// Preserve the raw authoritative response before publishing its receipt.
		// Billing failures never include bodies, auth data, or URLs in logs.
		sum := sha256.Sum256(observation.Evidence)
		if hex.EncodeToString(sum[:]) != receipt.EvidenceSHA256 {
			return imported, coordinatorUsageFailure("billing_evidence_mismatch")
		}
		if err := coordinatorUsageWriteEvidence(filepath.Join(b.evidenceDir, receipt.EvidenceSHA256+".json"), observation.Evidence, r.workspace); err != nil {
			return imported, err
		}
		data, err := json.Marshal(receipt)
		if err != nil {
			return imported, coordinatorUsageFailure("billing_receipt_encode_failed")
		}
		receiptSum := sha256.Sum256(data)
		path := filepath.Join(r.receiptDir, "billing-"+hex.EncodeToString(receiptSum[:])+".json")
		if err := coordinatorUsageWriteEvidence(path, data, r.workspace); err != nil {
			return imported, err
		}
		count, err := r.importFile(ctx, path)
		imported += count
		if err != nil && firstError == nil {
			firstError = err
		}
	}
	return imported, firstError
}
