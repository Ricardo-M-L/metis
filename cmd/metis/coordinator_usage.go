package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

const coordinatorUsageReceiptLimit = 2 << 20

// Explicit whitelist: embedded BeginUsageRequestInput contains a lease token
// and must never be marshaled to a CLI response or an adapter's stdin.
type coordinatorUsageMetadata struct {
	ClientRequestID    string                          `json:"client_request_id"`
	ItemID             string                          `json:"item_id"`
	Provider           string                          `json:"provider"`
	EndpointSHA256     string                          `json:"endpoint_sha256"`
	Model              string                          `json:"model"`
	RequestSHA256      string                          `json:"request_sha256"`
	ProviderRequestID  string                          `json:"provider_request_id,omitempty"`
	ResponseID         string                          `json:"response_id,omitempty"`
	ResponseStatusCode int                             `json:"response_status_code,omitempty"`
	Status             projectcoord.UsageRequestStatus `json:"status"`
	Usage              projectcoord.TokenUsage         `json:"usage"`
	UsageReported      bool                            `json:"usage_reported"`
	EstimatedUSD       float64                         `json:"estimated_usd"`
	ActualUSD          *float64                        `json:"actual_usd,omitempty"`
	BudgetUSD          float64                         `json:"budget_usd"`
	StartedAt          time.Time                       `json:"started_at"`
	UpdatedAt          time.Time                       `json:"updated_at"`
	ReceiptCount       int                             `json:"receipt_count"`
}

type coordinatorUsageListing struct {
	RunID                     string                     `json:"run_id"`
	Requests                  []coordinatorUsageMetadata `json:"requests"`
	Unresolved                int                        `json:"unresolved"`
	EstimatedUSD              float64                    `json:"estimated_usd"`
	ActualKnownUSD            float64                    `json:"actual_known_usd"`
	ActualCostUnknownRequests int                        `json:"actual_cost_unknown_requests"`
	BudgetSpentUSD            float64                    `json:"budget_spent_usd"`
}

func coordinatorUsagePublic(record projectcoord.UsageRequestRecord) coordinatorUsageMetadata {
	return coordinatorUsageMetadata{ClientRequestID: record.ClientRequestID, ItemID: record.ItemID, Provider: record.Provider, EndpointSHA256: record.EndpointSHA256, Model: record.Model, RequestSHA256: record.RequestSHA256, ProviderRequestID: record.ProviderRequestID, ResponseID: record.ResponseID, ResponseStatusCode: record.ResponseStatusCode, Status: record.Status, Usage: record.Usage, UsageReported: record.UsageReported, EstimatedUSD: record.EstimatedUSD, ActualUSD: record.ActualUSD, BudgetUSD: record.BudgetUSD, StartedAt: record.StartedAt, UpdatedAt: record.UpdatedAt, ReceiptCount: len(record.Receipts)}
}

func coordinatorUsageList(run projectcoord.Run) coordinatorUsageListing {
	result := coordinatorUsageListing{RunID: run.ID, Requests: make([]coordinatorUsageMetadata, 0, len(run.UsageRequests)), Unresolved: len(projectcoord.UnresolvedUsageRequests(run)), BudgetSpentUSD: run.SpentUSD}
	for _, record := range run.UsageRequests {
		result.Requests = append(result.Requests, coordinatorUsagePublic(record))
		result.EstimatedUSD += record.EstimatedUSD
		if record.ActualUSD == nil {
			result.ActualCostUnknownRequests++
		} else {
			result.ActualKnownUSD += *record.ActualUSD
		}
	}
	return result
}

func cmdProjectCoordinatorUsage(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "adapter-helper" {
		return cmdCoordinatorUsageAdapterHelper(ctx, args[1:])
	}
	if len(args) < 2 {
		return errors.New("usage: metis coordinator usage list|import|reconcile RUN [--cwd DIR] [--json]")
	}
	action, runID := args[0], args[1]
	if action != "list" && action != "import" && action != "reconcile" {
		return errors.New("unknown coordinator usage action; use list, import or reconcile")
	}
	fs := flag.NewFlagSet("coordinator usage "+action, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd := fs.String("cwd", "", "project working directory")
	jsonOut := fs.Bool("json", false, "emit usage metadata without worker credentials")
	var receiptFile, receiptDir, command, billingConfig string
	if action == "import" {
		fs.StringVar(&receiptFile, "receipt-file", "", "private authoritative provider receipt JSON outside the project")
	}
	if action == "reconcile" {
		fs.StringVar(&receiptDir, "receipt-dir", "", "private receipt inbox outside the project (default: METIS_HOME/coordinator-usage-receipts/RUN)")
		fs.StringVar(&command, "command", "", "optional private absolute adapter executable outside the project")
		fs.StringVar(&billingConfig, "billing-config", "", "private native provider billing configuration")
	}
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if len(fs.Args()) != 0 {
		return errors.New("coordinator usage does not accept extra positional arguments")
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	store := projectCoordinatorStore(projectcoord.WithProbeContext(ctx))
	run, err := store.Get(workspace, runID)
	if err != nil {
		return err
	}
	if action == "import" {
		data, _, err := coordinatorUsageReadPrivateFile(receiptFile, workspace, coordinatorUsageReceiptLimit, false)
		if err != nil {
			return err
		}
		receipts, err := coordinatorUsageDecodeReceipts(data)
		if err != nil {
			return err
		}
		if _, err := coordinatorUsageImport(ctx, store, workspace, runID, receipts); err != nil {
			return err
		}
	} else if action == "reconcile" {
		reconciler, err := newCoordinatorUsageReconciler(store, workspace, runID, config.Home(), receiptDir, command, nil)
		if err != nil {
			return err
		}
		if err := reconciler.configureBilling(billingConfig); err != nil {
			return err
		}
		summary, err := reconciler.CheckOnce(ctx)
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeCoordinatorJSON(summary)
		}
		fmt.Fprintf(os.Stdout, "Usage receipts: imported=%d unresolved=%d actual_known_usd=%.8f actual_cost_unknown_requests=%d\n", summary.Imported, summary.Unresolved, summary.ActualKnownUSD, summary.ActualCostUnknownRequests)
		return nil
	}
	if action == "import" {
		run, err = store.Get(workspace, runID)
		if err != nil {
			return err
		}
		if err := coordinatorBillingProjectJobUsage(run); err != nil {
			return err
		}
	}
	listing := coordinatorUsageList(run)
	if *jsonOut {
		return writeCoordinatorJSON(listing)
	}
	fmt.Fprintf(os.Stdout, "Provider requests: %d; unresolved usage: %d\nCatalogue estimate: %.8f USD; known receipt cost: %.8f USD; unknown actual costs: %d\n", len(listing.Requests), listing.Unresolved, listing.EstimatedUSD, listing.ActualKnownUSD, listing.ActualCostUnknownRequests)
	return nil
}

type coordinatorUsageError struct {
	code  string
	cause error
}

func (e *coordinatorUsageError) Error() string  { return "coordinator usage: " + e.code }
func (e *coordinatorUsageError) Unwrap() error  { return e.cause }
func coordinatorUsageFailure(code string) error { return &coordinatorUsageError{code: code} }

// Native receipt recovery requires owner-only POSIX modes and UID ownership.
// Windows support needs an equivalent ACL implementation before any private
// receipt, adapter or billing credential can be accessed there.
func coordinatorUsagePlatformError() error {
	if goruntime.GOOS == "windows" {
		return coordinatorUsageFailure("unsupported_platform_windows")
	}
	return nil
}

func coordinatorUsageImport(ctx context.Context, store *projectcoord.Store, workspace, runID string, receipts []projectcoord.UsageReceipt) (int, error) {
	imported := 0
	for _, receipt := range receipts {
		if err := ctx.Err(); err != nil {
			return imported, err
		}
		if _, err := store.ImportUsageReceipt(workspace, runID, receipt); err != nil {
			message := err.Error()
			code := "receipt_import_failed"
			if strings.Contains(message, "conflict") || strings.Contains(message, "more than one request") {
				code = "receipt_conflict"
			} else if strings.Contains(message, "does not match") {
				code = "receipt_scope_mismatch"
			}
			return imported, coordinatorUsageFailure(code)
		}
		imported++
	}
	return imported, nil
}

func coordinatorUsageDecodeReceipts(data []byte) ([]projectcoord.UsageReceipt, error) {
	if len(data) > coordinatorUsageReceiptLimit {
		return nil, coordinatorUsageFailure("receipt_size_limit")
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || (data[0] != '[' && data[0] != '{') {
		return nil, coordinatorUsageFailure("invalid_receipt_json")
	}
	if err := coordinatorUsageUniqueJSON(data); err != nil {
		return nil, coordinatorUsageFailure("invalid_receipt_json")
	}
	if err := coordinatorUsageExplicitUsage(data); err != nil {
		return nil, coordinatorUsageFailure("invalid_receipt_json")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result []projectcoord.UsageReceipt
	if data[0] == '[' {
		if err := decoder.Decode(&result); err != nil {
			return nil, coordinatorUsageFailure("invalid_receipt_json")
		}
	} else {
		var receipt projectcoord.UsageReceipt
		if err := decoder.Decode(&receipt); err != nil {
			return nil, coordinatorUsageFailure("invalid_receipt_json")
		}
		result = []projectcoord.UsageReceipt{receipt}
	}
	if len(result) > 10000 {
		return nil, coordinatorUsageFailure("receipt_count_limit")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, coordinatorUsageFailure("invalid_receipt_json")
	}
	return result, nil
}

func coordinatorUsageExplicitUsage(data []byte) error {
	var records []json.RawMessage
	if data[0] == '[' {
		if err := json.Unmarshal(data, &records); err != nil {
			return err
		}
	} else {
		records = []json.RawMessage{data}
	}
	for _, raw := range records {
		var receipt map[string]json.RawMessage
		if err := json.Unmarshal(raw, &receipt); err != nil || receipt == nil {
			return errors.New("receipt must be an object")
		}
		var usage map[string]json.RawMessage
		allowed := map[string]bool{"receipt_id": true, "evidence_sha256": true, "authority": true, "provider": true, "endpoint_sha256": true, "model": true, "client_request_id": true, "provider_request_id": true, "response_id": true, "usage": true, "actual_usd": true, "supersedes_receipt_id": true}
		for field := range receipt {
			if !allowed[field] {
				return errors.New("receipt requires exact JSON field names")
			}
		}
		if err := json.Unmarshal(receipt["usage"], &usage); err != nil || usage == nil {
			return errors.New("receipt requires explicit absolute usage")
		}
		for field := range usage {
			if field != "input" && field != "output" && field != "cache_read" && field != "cache_write" {
				return errors.New("receipt requires exact token field names")
			}
		}
		for _, field := range []string{"input", "output", "cache_read", "cache_write"} {
			value, exists := usage[field]
			if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return errors.New("receipt requires explicit token fields")
			}
		}
	}
	return nil
}

// Duplicate keys (including inside usage) could otherwise silently replace an
// earlier authoritative field. Inspect tokens before decoding typed values.
func coordinatorUsageUniqueJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("JSON nesting limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				name = strings.ToLower(name)
				if !ok || keys[name] {
					return errors.New("duplicate JSON field")
				}
				keys[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("extra JSON value")
	}
	return nil
}

func coordinatorUsageOwned(info os.FileInfo) bool {
	data := reflect.ValueOf(info.Sys())
	if data.Kind() == reflect.Pointer {
		data = data.Elem()
	}
	if !data.IsValid() || data.Kind() != reflect.Struct {
		return false
	}
	uid := data.FieldByName("Uid")
	return uid.IsValid() && uid.CanUint() && os.Getuid() >= 0 && uid.Uint() == uint64(os.Getuid())
}

func coordinatorUsageOutsideWorkspace(path, workspace string) bool {
	relative, err := filepath.Rel(workspace, path)
	return err == nil && relative != "." && (relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func coordinatorUsageReadPrivateFile(path, workspace string, limit int64, executable bool) ([]byte, os.FileInfo, error) {
	if err := coordinatorUsagePlatformError(); err != nil {
		return nil, nil, err
	}
	if !filepath.IsAbs(path) {
		return nil, nil, coordinatorUsageFailure("private_file_requires_absolute_path")
	}
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil, &coordinatorUsageError{code: "private_file_missing", cause: os.ErrNotExist}
	}
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || !coordinatorUsageOwned(before) || before.Size() > limit || (executable && before.Mode().Perm()&0100 == 0) || (!executable && before.Mode().Perm() != 0600) {
		return nil, nil, coordinatorUsageFailure("unsafe_private_file")
	}
	real, err := filepath.EvalSymlinks(path)
	if os.IsNotExist(err) {
		return nil, nil, &coordinatorUsageError{code: "private_file_missing", cause: os.ErrNotExist}
	}
	if err != nil || !coordinatorUsageOutsideWorkspace(real, workspace) {
		return nil, nil, coordinatorUsageFailure("private_file_must_be_outside_workspace")
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil, &coordinatorUsageError{code: "private_file_missing", cause: os.ErrNotExist}
	}
	if err != nil {
		return nil, nil, coordinatorUsageFailure("private_file_read_failed")
	}
	defer file.Close()
	observed, err := file.Stat()
	if err != nil || !os.SameFile(before, observed) {
		return nil, nil, coordinatorUsageFailure("private_file_changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, nil, coordinatorUsageFailure("private_file_read_failed")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, nil, coordinatorUsageFailure("private_file_changed")
	}
	return data, after, nil
}
