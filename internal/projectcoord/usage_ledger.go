package projectcoord

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
)

// UsageRequestStatus describes one actual provider request, not a work item or
// an entire retry loop. A killed request stays unresolved until a receipt from
// the bound provider account supplies its usage; no tokens are invented.
type UsageRequestStatus string

const (
	UsageRequestStarted     UsageRequestStatus = "started"
	UsageRequestCompleted   UsageRequestStatus = "completed"
	UsageRequestInterrupted UsageRequestStatus = "interrupted"
	UsageRequestRejected    UsageRequestStatus = "rejected"
	UsageRequestReconciled  UsageRequestStatus = "reconciled"
)

// ErrUsageUnresolved is a safe admission failure for a work item's unfinished
// physical provider request. It contains no worker token, request ID, prompt or
// provider response text. Transport observers preserve its classification
// without treating it as a retryable network failure.
var ErrUsageUnresolved = usageUnresolvedError{}

type usageUnresolvedError struct{}

func (usageUnresolvedError) Error() string {
	return "work item has unresolved provider usage; reconcile its request receipts before dispatching another provider request"
}

func (usageUnresolvedError) UsageFailureCode() string { return "provider_usage_unresolved" }

// TokenUsage is an absolute provider snapshot. Input excludes separately
// reported cache-read and cache-write tokens, as in METIS's pricing catalogue.
type TokenUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}

// UsagePriceSnapshot preserves the catalogue used before the request starts.
// It is an estimate, not evidence of an actual charge on a provider bill.
type UsagePriceSnapshot struct {
	InputPerMillionUSD      float64 `json:"input_per_million_usd"`
	OutputPerMillionUSD     float64 `json:"output_per_million_usd"`
	CacheReadPerMillionUSD  float64 `json:"cache_read_per_million_usd"`
	CacheWritePerMillionUSD float64 `json:"cache_write_per_million_usd"`
	Source                  string  `json:"source"`
}

// BeginUsageRequestInput contains only non-secret identity and pricing data.
// EndpointSHA256 hashes the selected endpoint, never an API key. RequestSHA256
// hashes the exact request bytes, without persisting prompts or headers.
type BeginUsageRequestInput struct {
	ClientRequestID string             `json:"client_request_id"`
	ItemID          string             `json:"item_id"`
	Worker          string             `json:"worker"`
	ClaimToken      string             `json:"claim_token"`
	Provider        string             `json:"provider"`
	EndpointSHA256  string             `json:"endpoint_sha256"`
	Model           string             `json:"model"`
	RequestSHA256   string             `json:"request_sha256"`
	Prices          UsagePriceSnapshot `json:"prices"`
}

// UsageRequestRecord is written before network I/O. The graph attempt binding
// never changes, even when a replacement worker has taken over the work item.
type UsageRequestRecord struct {
	BeginUsageRequestInput
	Status             UsageRequestStatus `json:"status"`
	ProviderRequestID  string             `json:"provider_request_id,omitempty"`
	ResponseID         string             `json:"response_id,omitempty"`
	ResponseStatusCode int                `json:"response_status_code,omitempty"`
	Usage              TokenUsage         `json:"usage"`
	UsageReported      bool               `json:"usage_reported"`
	EstimatedUSD       float64            `json:"estimated_usd"`
	ActualUSD          *float64           `json:"actual_usd,omitempty"`
	BudgetUSD          float64            `json:"budget_usd"`
	StartedAt          time.Time          `json:"started_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
	Receipts           []UsageReceipt     `json:"receipts,omitempty"`
	Sequence           uint64             `json:"sequence,omitempty"`
	AccountingVersion  int                `json:"accounting_version,omitempty"`
}

// UsageSnapshotInput is an absolute observation from the original
// request. It may arrive after its worker lease has expired, but cannot begin a
// new request or write task outputs using that expired lease. Sequence is the
// request observer's serialized observation order. Once used, an unsequenced or
// older observation cannot replace the latest provider correction.
type UsageSnapshotInput struct {
	ClientRequestID    string             `json:"client_request_id"`
	ItemID             string             `json:"item_id"`
	Worker             string             `json:"worker"`
	ClaimToken         string             `json:"claim_token"`
	ProviderRequestID  string             `json:"provider_request_id,omitempty"`
	ResponseID         string             `json:"response_id,omitempty"`
	ResponseStatusCode int                `json:"response_status_code,omitempty"`
	Usage              TokenUsage         `json:"usage"`
	UsageReported      bool               `json:"usage_reported"`
	Status             UsageRequestStatus `json:"status"`
	Sequence           uint64             `json:"sequence,omitempty"`
}

// UsageReceipt is an operator-imported or trusted-adapter provider receipt.
// Authority names the source of the evidence; it is not a signature check.
// Matching IDs and provider/endpoint/model scope prevent unrelated bills from
// being assigned to an orphan request. ActualUSD is optional and kept separate
// from the catalogue estimate. All token fields are absolute, not deltas. A
// correction to settled receipt evidence must explicitly supersede the current
// final receipt, so an older export cannot silently roll back newer billing.
type UsageReceipt struct {
	ReceiptID           string     `json:"receipt_id"`
	EvidenceSHA256      string     `json:"evidence_sha256"`
	Authority           string     `json:"authority"`
	Provider            string     `json:"provider"`
	EndpointSHA256      string     `json:"endpoint_sha256"`
	Model               string     `json:"model"`
	ClientRequestID     string     `json:"client_request_id,omitempty"`
	ProviderRequestID   string     `json:"provider_request_id,omitempty"`
	ResponseID          string     `json:"response_id,omitempty"`
	Usage               TokenUsage `json:"usage"`
	ActualUSD           *float64   `json:"actual_usd,omitempty"`
	SupersedesReceiptID string     `json:"supersedes_receipt_id,omitempty"`
}

// NewUsageRequestID fails closed when OS randomness is unavailable. It has no
// timestamp fallback because a provider correlation ID must be unambiguous.
func NewUsageRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate provider request ID: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

// BeginUsageRequest atomically reserves an identity under a live claim. The
// caller must not contact the provider unless this transaction succeeds.
func (s *Store) BeginUsageRequest(workspace, runID string, input BeginUsageRequestInput) (UsageRequestRecord, error) {
	if err := validateUsageBegin(input); err != nil {
		return UsageRequestRecord{}, err
	}
	var result UsageRequestRecord
	updated, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		if _, exists := usageRequestByID(run, input.ClientRequestID); exists {
			// Replaying Begin could repeat an already dispatched request. Only
			// snapshot/receipt writes are idempotent; dispatch reservation is not.
			return false, errors.New("provider request ID has already been reserved")
		}
		item, err := run.item(input.ItemID)
		if err != nil {
			return false, err
		}
		if err := item.assertClaim(input.Worker, input.ClaimToken); err != nil {
			return false, err
		}
		if _, err := usageAttempt(item, input.Worker, input.ClaimToken); err != nil {
			return false, err
		}
		now := s.now()
		if !item.Lease.ExpiresAt.After(now) || (!WorkDeadline(*run, *item).IsZero() && !WorkDeadline(*run, *item).After(now)) {
			return false, errors.New("provider dispatch requires a live, unexpired worker lease and task deadline")
		}
		// The reservation and this check share the cross-process run lock.
		// Include earlier attempts and changed payloads: a replacement claim
		// cannot make an unsettled physical request disappear or infer that its
		// missing counters mean zero consumption.
		if err := run.assertUsageResolved(input.ItemID); err != nil {
			return false, err
		}
		result = UsageRequestRecord{BeginUsageRequestInput: input, Status: UsageRequestStarted, StartedAt: now, UpdatedAt: now, AccountingVersion: 2}
		run.UsageRequests = append(run.UsageRequests, result)
		run.UpdatedAt = now
		return true, nil
	})
	if err != nil {
		return UsageRequestRecord{}, err
	}
	// withRun reconciles once more after its callback. A lease/deadline may
	// cross its boundary while the reservation is being synced to disk. Never
	// authorize network I/O using the callback's now-stale claim snapshot.
	item, err := updated.item(input.ItemID)
	if err != nil {
		return UsageRequestRecord{}, err
	}
	if err := item.assertClaim(input.Worker, input.ClaimToken); err != nil {
		return UsageRequestRecord{}, err
	}
	now := s.now()
	if !item.Lease.ExpiresAt.After(now) || (!WorkDeadline(updated, *item).IsZero() && !WorkDeadline(updated, *item).After(now)) {
		return UsageRequestRecord{}, errors.New("worker lease or task deadline expired while reserving provider request")
	}
	stored, found := usageRequestByID(&updated, input.ClientRequestID)
	if !found {
		return UsageRequestRecord{}, errors.New("provider request reservation was not persisted")
	}
	return cloneUsageRequest(*stored), nil
}

// RecordUsageSnapshot persists the latest absolute snapshot. Providers can
// revise estimates down or disclose cache classification late; the same locked
// transaction applies its signed cost difference to all durable budget totals.
func (s *Store) RecordUsageSnapshot(workspace, runID string, input UsageSnapshotInput) (UsageRequestRecord, error) {
	if err := validateTokenUsage(input.Usage); err != nil {
		return UsageRequestRecord{}, err
	}
	if !validUsageStatus(input.Status) || input.Status == UsageRequestReconciled {
		return UsageRequestRecord{}, errors.New("invalid observed provider request status")
	}
	if err := validateRemoteUsageIDs(input.ProviderRequestID, input.ResponseID); err != nil {
		return UsageRequestRecord{}, err
	}
	if err := validateUsageResponseStatus(input.ResponseStatusCode); err != nil {
		return UsageRequestRecord{}, err
	}
	var result UsageRequestRecord
	_, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		record, found := usageRequestByID(run, input.ClientRequestID)
		if !found {
			return false, errors.New("provider request has not been reserved")
		}
		if record.ItemID != input.ItemID || record.Worker != input.Worker || record.ClaimToken != input.ClaimToken {
			return false, errors.New("provider usage observation does not belong to this worker attempt")
		}
		if err := validateUsageTransition(record.Status, input.Status); err != nil {
			return false, err
		}
		candidate := cloneUsageRequest(*record)
		if err := bindRemoteUsageIDs(&candidate, input.ProviderRequestID, input.ResponseID); err != nil {
			return false, err
		}
		if input.ResponseStatusCode != 0 {
			if candidate.ResponseStatusCode != 0 && candidate.ResponseStatusCode != input.ResponseStatusCode {
				return false, errors.New("physical provider response status cannot change")
			}
			candidate.ResponseStatusCode = input.ResponseStatusCode
		}
		candidate.Status = input.Status
		if input.UsageReported {
			candidate.Usage, candidate.UsageReported = input.Usage, true
		}
		if (record.Status == UsageRequestCompleted || record.Status == UsageRequestRejected) && (candidate.Usage != record.Usage || candidate.UsageReported != record.UsageReported) {
			return false, errors.New("terminal provider usage requires authoritative receipt correction")
		}
		candidate.Sequence = input.Sequence
		if input.Sequence != 0 || record.Sequence != 0 {
			if input.Sequence == 0 || input.Sequence < record.Sequence {
				return false, errors.New("provider usage observation sequence is stale")
			}
			if input.Sequence == record.Sequence {
				if !usageRecordsEquivalent(*record, candidate) {
					return false, errors.New("provider usage observation sequence conflicts with persisted contents")
				}
				result = *record
				return false, nil
			}
		}
		if candidate.Status == UsageRequestRejected && candidate.Usage != (TokenUsage{}) {
			return false, errors.New("rejected provider request cannot have consumed tokens")
		}
		if err := updateUsageCosts(&candidate); err != nil {
			return false, err
		}
		if usageRecordsEquivalent(*record, candidate) {
			result = *record
			return false, nil
		}
		candidate.UpdatedAt = s.now()
		if err := applyUsageRecord(run, candidate); err != nil {
			return false, err
		}
		run.UpdatedAt = s.now()
		result = candidate
		return true, nil
	})
	return cloneUsageRequest(result), err
}

// ImportUsageReceipt resolves missing usage without making another inference
// request. A repeated identical receipt is a no-op; a reused ID with changed
// identity, evidence, tokens, or actual cost is an error.
func (s *Store) ImportUsageReceipt(workspace, runID string, receipt UsageReceipt) (UsageRequestRecord, error) {
	if err := validateUsageReceipt(receipt); err != nil {
		return UsageRequestRecord{}, err
	}
	var result UsageRequestRecord
	_, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		for i := range run.UsageRequests {
			for _, previous := range run.UsageRequests[i].Receipts {
				if previous.ReceiptID != receipt.ReceiptID {
					continue
				}
				if !usageReceiptsEquivalent(previous, receipt) {
					return false, errors.New("provider receipt ID conflicts with previously imported evidence")
				}
				result = run.UsageRequests[i]
				return false, nil
			}
		}
		record, err := matchUsageReceipt(run, receipt)
		if err != nil {
			return false, err
		}
		if len(record.Receipts) == 0 {
			if receipt.SupersedesReceiptID != "" {
				return false, errors.New("provider receipt supersession has no prior evidence")
			}
		} else {
			last := record.Receipts[len(record.Receipts)-1]
			if receipt.SupersedesReceiptID != "" {
				if receipt.SupersedesReceiptID != last.ReceiptID {
					return false, errors.New("provider receipt supersession does not match current evidence")
				}
			} else {
				if err := assertUsageNotDecreasing(record.Usage, receipt.Usage); err != nil {
					return false, err
				}
				if record.ActualUSD != nil && receipt.ActualUSD != nil && *receipt.ActualUSD < *record.ActualUSD {
					return false, errors.New("absolute provider receipt cost correction requires current evidence supersession")
				}
			}
		}
		candidate := cloneUsageRequest(*record)
		if err := bindRemoteUsageIDs(&candidate, receipt.ProviderRequestID, receipt.ResponseID); err != nil {
			return false, err
		}
		if receipt.ActualUSD != nil {
			actual := *receipt.ActualUSD
			candidate.ActualUSD = &actual
		}
		candidate.Status, candidate.Usage, candidate.UsageReported = UsageRequestReconciled, receipt.Usage, true
		candidate.Receipts = append(candidate.Receipts, cloneUsageReceipt(receipt))
		if err := updateUsageCosts(&candidate); err != nil {
			return false, err
		}
		candidate.UpdatedAt = s.now()
		if err := applyUsageRecord(run, candidate); err != nil {
			return false, err
		}
		run.UpdatedAt = s.now()
		run.addEvent(s.now(), "usage_reconciled", candidate.ItemID, "Imported matching provider usage evidence; actual receipt cost and catalogue estimate remain separate.", candidate.Worker)
		result = candidate
		return true, nil
	})
	return cloneUsageRequest(result), err
}

// UnresolvedUsageRequests includes dispatched requests that have no observed
// completion. It intentionally reports unknown usage rather than estimating
// it from elapsed time, prompt size, or the maximum output-token setting.
func UnresolvedUsageRequests(run Run) []UsageRequestRecord {
	var requests []UsageRequestRecord
	for _, record := range run.UsageRequests {
		// A gateway rejection can follow upstream work that consumed tokens.
		// Without an explicit usage field it is unknown, never evidence of a
		// zero charge. An authoritative zero receipt can resolve it later.
		if usageRequestUnresolved(record) {
			requests = append(requests, cloneUsageRequest(record))
		}
	}
	return requests
}

func usageRequestUnresolved(record UsageRequestRecord) bool {
	return record.Status == UsageRequestStarted || record.Status == UsageRequestInterrupted || (record.Status != UsageRequestReconciled && !record.UsageReported)
}

func (r *Run) assertUsageResolved(itemID string) error {
	for _, record := range r.UsageRequests {
		if record.ItemID == itemID && usageRequestUnresolved(record) {
			return ErrUsageUnresolved
		}
	}
	return nil
}

func usageRequestByID(run *Run, id string) (*UsageRequestRecord, bool) {
	for i := range run.UsageRequests {
		if run.UsageRequests[i].ClientRequestID == id {
			return &run.UsageRequests[i], true
		}
	}
	return nil, false
}

func usageAttempt(item *WorkItem, worker, token string) (*AttemptUsage, error) {
	for i := range item.Usage {
		if item.Usage[i].Worker == worker && item.Usage[i].Token == token {
			return &item.Usage[i], nil
		}
	}
	return nil, errors.New("provider usage claim token does not belong to this worker attempt")
}

func applyUsageRecord(run *Run, record UsageRequestRecord) error {
	// withRun may persist automatic lease recovery even when a mutation
	// returns an error. Validate all accounting on a detached copy so a cost
	// overflow or inconsistent attempt can never save a half-updated ledger.
	candidate := cloneRun(*run)
	stored, found := usageRequestByID(&candidate, record.ClientRequestID)
	if !found {
		return errors.New("provider request has not been reserved")
	}
	*stored = cloneUsageRequest(record)
	if err := reconcileUsageAttempt(&candidate, record.ItemID, record.Worker, record.ClaimToken); err != nil {
		return err
	}
	*run = candidate
	return nil
}

func reconcileUsageAttempt(run *Run, itemID, worker, token string) error {
	item, err := run.item(itemID)
	if err != nil {
		return err
	}
	attempt, err := usageAttempt(item, worker, token)
	if err != nil {
		return err
	}
	// Migrate a pre-ledger attempt lazily. Zero-valued new fields are fully
	// backward compatible with state written by earlier METIS versions.
	if attempt.LegacySpentUSD == 0 && attempt.RequestSpentUSD == 0 && attempt.SpentUSD > 0 {
		attempt.LegacySpentUSD = attempt.SpentUSD
	}
	requestTotal := 0.0
	for _, record := range run.UsageRequests {
		if record.ItemID == itemID && record.Worker == worker && record.ClaimToken == token {
			requestTotal += record.BudgetUSD
		}
	}
	if !validUsageUSD(requestTotal) {
		return errors.New("provider request ledger cost overflow")
	}
	next := math.Max(attempt.LegacySpentUSD, requestTotal)
	attempt.RequestSpentUSD, attempt.SpentUSD = requestTotal, next
	// Recompute aggregate projections instead of repeatedly subtracting
	// floating-point deltas. Refunding the last request must produce exact
	// zero, not a negative rounding residual or a phantom budget balance.
	itemTotal := 0.0
	for _, usage := range item.Usage {
		itemTotal += usage.SpentUSD
	}
	if !validUsageUSD(itemTotal) {
		return errors.New("provider usage item ledger cost overflow")
	}
	item.SpentUSD = itemTotal
	runTotal := 0.0
	for _, work := range run.Items {
		runTotal += work.SpentUSD
	}
	if !validUsageUSD(runTotal) {
		return errors.New("provider usage run ledger cost overflow")
	}
	run.SpentUSD = runTotal
	return nil
}

func matchUsageReceipt(run *Run, receipt UsageReceipt) (*UsageRequestRecord, error) {
	var matched *UsageRequestRecord
	for i := range run.UsageRequests {
		record := &run.UsageRequests[i]
		if record.Provider != receipt.Provider || record.EndpointSHA256 != receipt.EndpointSHA256 || record.Model != receipt.Model {
			continue
		}
		if receipt.ClientRequestID != "" {
			if record.ClientRequestID != receipt.ClientRequestID {
				continue
			}
		} else {
			// Without a client correlation ID, remote IDs must have been
			// observed and persisted before the provider receipt arrives. One
			// known ID is sufficient; a receipt may reveal its other ID for the
			// first time. Any already known conflicting ID is rejected below.
			knownRequest := receipt.ProviderRequestID != "" && record.ProviderRequestID != "" && receipt.ProviderRequestID == record.ProviderRequestID
			knownResponse := receipt.ResponseID != "" && record.ResponseID != "" && receipt.ResponseID == record.ResponseID
			if !knownRequest && !knownResponse {
				continue
			}
		}
		if record.ProviderRequestID != "" && receipt.ProviderRequestID != "" && record.ProviderRequestID != receipt.ProviderRequestID {
			return nil, errors.New("provider receipt request ID conflicts with the reserved request")
		}
		if record.ResponseID != "" && receipt.ResponseID != "" && record.ResponseID != receipt.ResponseID {
			return nil, errors.New("provider receipt response ID conflicts with the reserved request")
		}
		if matched != nil {
			return nil, errors.New("provider receipt matches more than one request")
		}
		matched = record
	}
	if matched == nil {
		return nil, errors.New("provider receipt does not match a reserved request in this provider/endpoint/model scope")
	}
	return matched, nil
}

func validateUsageBegin(input BeginUsageRequestInput) error {
	if !validUsageHex(input.ClientRequestID, 16) {
		return errors.New("client provider request ID must contain 128 bits of hexadecimal identity")
	}
	if !validID(input.ItemID) || !validUsageText(input.Worker, 256) || !validUsageText(input.ClaimToken, 256) {
		return errors.New("provider usage requires a valid item, worker, and claim token")
	}
	if !validUsageText(input.Provider, 128) || !validUsageText(input.Model, 256) || !validUsageHex(input.EndpointSHA256, 32) || !validUsageHex(input.RequestSHA256, 32) {
		return errors.New("provider usage requires provider/model identity and SHA-256 endpoint/request bindings")
	}
	return validateUsagePrices(input.Prices)
}

func validateUsagePrices(prices UsagePriceSnapshot) error {
	if !validUsageText(prices.Source, 256) {
		return errors.New("provider pricing snapshot requires an estimate source")
	}
	for _, price := range []float64{prices.InputPerMillionUSD, prices.OutputPerMillionUSD, prices.CacheReadPerMillionUSD, prices.CacheWritePerMillionUSD} {
		if !validUsageUSD(price) {
			return errors.New("provider pricing snapshot must contain finite, non-negative USD rates")
		}
	}
	return nil
}

func validateTokenUsage(usage TokenUsage) error {
	if usage.Input < 0 || usage.Output < 0 || usage.CacheRead < 0 || usage.CacheWrite < 0 {
		return errors.New("absolute provider token usage cannot be negative")
	}
	maxInt := int(^uint(0) >> 1)
	if usage.Input > maxInt-usage.CacheRead || usage.Input+usage.CacheRead > maxInt-usage.CacheWrite {
		return errors.New("absolute provider input token total overflows the native integer range")
	}
	return nil
}

func assertUsageNotDecreasing(before, after TokenUsage) error {
	// A provider may first report all input as uncached, then disclose its
	// cache split. Compare total input rather than taking independent bucket
	// maxima, which would double charge those same input tokens.
	beforeTotal := before.Input + before.CacheRead + before.CacheWrite
	afterTotal := after.Input + after.CacheRead + after.CacheWrite
	if afterTotal < beforeTotal || after.Output < before.Output {
		return errors.New("absolute provider token usage cannot decrease")
	}
	return nil
}

func validUsageStatus(status UsageRequestStatus) bool {
	switch status {
	case UsageRequestStarted, UsageRequestCompleted, UsageRequestInterrupted, UsageRequestRejected, UsageRequestReconciled:
		return true
	default:
		return false
	}
}

func validateUsageTransition(before, after UsageRequestStatus) error {
	if before == UsageRequestReconciled || (before == UsageRequestCompleted && after != UsageRequestCompleted) || (before == UsageRequestRejected && after != UsageRequestRejected) || (before == UsageRequestInterrupted && after == UsageRequestStarted) {
		return errors.New("provider request status cannot regress or change after reconciliation")
	}
	return nil
}

func validateRemoteUsageIDs(providerID, responseID string) error {
	for _, id := range []string{providerID, responseID} {
		if id != "" && !validUsageText(id, 512) {
			return errors.New("invalid provider request or response ID")
		}
	}
	return nil
}

func validateUsageResponseStatus(status int) error {
	if status != 0 && (status < 100 || status > 599) {
		return errors.New("invalid physical provider HTTP response status")
	}
	return nil
}

func bindRemoteUsageIDs(record *UsageRequestRecord, providerID, responseID string) error {
	if providerID != "" {
		if record.ProviderRequestID != "" && record.ProviderRequestID != providerID {
			return errors.New("provider request ID cannot change")
		}
		record.ProviderRequestID = providerID
	}
	if responseID != "" {
		if record.ResponseID != "" && record.ResponseID != responseID {
			return errors.New("provider response ID cannot change")
		}
		record.ResponseID = responseID
	}
	return nil
}

func validateUsageReceipt(receipt UsageReceipt) error {
	if !validUsageText(receipt.ReceiptID, 256) || !validUsageText(receipt.Authority, 256) || !validUsageHex(receipt.EvidenceSHA256, 32) {
		return errors.New("provider receipt requires a bounded ID, authority, and SHA-256 evidence digest")
	}
	if !validUsageText(receipt.Provider, 128) || !validUsageText(receipt.Model, 256) || !validUsageHex(receipt.EndpointSHA256, 32) {
		return errors.New("provider receipt requires provider/endpoint/model scope")
	}
	if receipt.ClientRequestID != "" && !validUsageHex(receipt.ClientRequestID, 16) {
		return errors.New("invalid provider receipt client request ID")
	}
	if receipt.ClientRequestID == "" && receipt.ProviderRequestID == "" && receipt.ResponseID == "" {
		return errors.New("provider receipt requires a client ID or an already recorded provider request/response ID")
	}
	if receipt.SupersedesReceiptID != "" && (!validUsageText(receipt.SupersedesReceiptID, 256) || receipt.SupersedesReceiptID == receipt.ReceiptID) {
		return errors.New("provider receipt supersession requires a different bounded receipt ID")
	}
	if err := validateRemoteUsageIDs(receipt.ProviderRequestID, receipt.ResponseID); err != nil {
		return err
	}
	if err := validateTokenUsage(receipt.Usage); err != nil {
		return err
	}
	if receipt.ActualUSD != nil && !validUsageUSD(*receipt.ActualUSD) {
		return errors.New("actual provider receipt cost must be a finite, non-negative USD amount")
	}
	return nil
}

func updateUsageCosts(record *UsageRequestRecord) error {
	p := record.Prices
	u := record.Usage
	record.EstimatedUSD = (float64(u.Input)*p.InputPerMillionUSD + float64(u.Output)*p.OutputPerMillionUSD + float64(u.CacheRead)*p.CacheReadPerMillionUSD + float64(u.CacheWrite)*p.CacheWritePerMillionUSD) / 1_000_000
	if !validUsageUSD(record.EstimatedUSD) || !validUsageUSD(record.BudgetUSD) {
		return errors.New("provider usage estimate overflow")
	}
	// An absolute correction replaces an earlier estimate. Once the provider
	// supplies actual cost it is authoritative, including a zero charge.
	record.AccountingVersion = 2
	record.BudgetUSD = record.EstimatedUSD
	if record.ActualUSD != nil {
		if !validUsageUSD(*record.ActualUSD) {
			return errors.New("invalid actual provider receipt cost")
		}
		record.BudgetUSD = *record.ActualUSD
	}
	return nil
}

func validUsageUSD(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validUsageHex(value string, size int) bool {
	if len(value) != size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validUsageText(value string, limit int) bool {
	if value == "" || len(value) > limit || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func usageReceiptsEquivalent(a, b UsageReceipt) bool {
	aJSON, _ := json.Marshal(a)
	bJSON, _ := json.Marshal(b)
	return sha256.Sum256(aJSON) == sha256.Sum256(bJSON)
}

func usageRecordsEquivalent(a, b UsageRequestRecord) bool {
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	aJSON, _ := json.Marshal(a)
	bJSON, _ := json.Marshal(b)
	return sha256.Sum256(aJSON) == sha256.Sum256(bJSON)
}

func cloneUsageReceipt(receipt UsageReceipt) UsageReceipt {
	if receipt.ActualUSD != nil {
		actual := *receipt.ActualUSD
		receipt.ActualUSD = &actual
	}
	return receipt
}

func cloneUsageRequest(record UsageRequestRecord) UsageRequestRecord {
	if record.ActualUSD != nil {
		actual := *record.ActualUSD
		record.ActualUSD = &actual
	}
	if record.Receipts != nil {
		record.Receipts = append([]UsageReceipt(nil), record.Receipts...)
		for i := range record.Receipts {
			record.Receipts[i] = cloneUsageReceipt(record.Receipts[i])
		}
	}
	return record
}

func cloneUsageRequests(requests []UsageRequestRecord) []UsageRequestRecord {
	if requests == nil {
		return nil
	}
	result := make([]UsageRequestRecord, len(requests))
	for i := range requests {
		result[i] = cloneUsageRequest(requests[i])
	}
	return result
}

// validateUsageLedger is called while loading a run. It verifies request
// ownership and receipt fingerprints without changing old zero-field state.
func validateUsageLedger(run Run) error {
	ids, receipts := map[string]bool{}, map[string]bool{}
	requestTotals := map[string]float64{}
	for _, record := range run.UsageRequests {
		if err := validateUsageBegin(record.BeginUsageRequestInput); err != nil {
			return err
		}
		if ids[record.ClientRequestID] {
			return errors.New("duplicate provider request ID in persisted ledger")
		}
		ids[record.ClientRequestID] = true
		if !validUsageStatus(record.Status) || record.StartedAt.IsZero() || record.UpdatedAt.Before(record.StartedAt) {
			return errors.New("invalid persisted provider request lifecycle")
		}
		if err := validateTokenUsage(record.Usage); err != nil {
			return err
		}
		if !record.UsageReported && record.Usage != (TokenUsage{}) {
			return errors.New("persisted provider request has tokens without a reported usage observation")
		}
		if err := validateRemoteUsageIDs(record.ProviderRequestID, record.ResponseID); err != nil {
			return err
		}
		if err := validateUsageResponseStatus(record.ResponseStatusCode); err != nil {
			return err
		}
		if record.Status == UsageRequestRejected && record.Usage != (TokenUsage{}) {
			return errors.New("persisted rejected provider request has token usage")
		}
		candidate := record
		if err := updateUsageCosts(&candidate); err != nil {
			return err
		}
		if record.AccountingVersion != 0 && record.AccountingVersion != 2 {
			return errors.New("unsupported persisted provider usage accounting version")
		}
		// Pre-correction records used a high-water estimate. Keep those states
		// readable; their next ordered write migrates accounting atomically.
		legacyCostValid := record.AccountingVersion == 0 && record.BudgetUSD >= candidate.EstimatedUSD && (record.ActualUSD == nil || record.BudgetUSD >= *record.ActualUSD)
		if candidate.EstimatedUSD != record.EstimatedUSD || (candidate.BudgetUSD != record.BudgetUSD && !legacyCostValid) {
			return errors.New("persisted provider request costs disagree with its token/pricing evidence")
		}
		item, err := run.item(record.ItemID)
		if err != nil {
			return err
		}
		if _, err := usageAttempt(item, record.Worker, record.ClaimToken); err != nil {
			return err
		}
		key := record.ItemID + "\x00" + record.Worker + "\x00" + record.ClaimToken
		requestTotals[key] += record.BudgetUSD
		if !validUsageUSD(requestTotals[key]) {
			return errors.New("persisted provider attempt ledger cost overflow")
		}
		previousReceiptUsage := TokenUsage{}
		var previousActual *float64
		for i, receipt := range record.Receipts {
			if err := validateUsageReceipt(receipt); err != nil {
				return err
			}
			if receipts[receipt.ReceiptID] {
				return errors.New("duplicate provider receipt ID in persisted ledger")
			}
			receipts[receipt.ReceiptID] = true
			if receipt.Provider != record.Provider || receipt.EndpointSHA256 != record.EndpointSHA256 || receipt.Model != record.Model || (receipt.ClientRequestID != "" && receipt.ClientRequestID != record.ClientRequestID) || (receipt.ProviderRequestID != "" && receipt.ProviderRequestID != record.ProviderRequestID) || (receipt.ResponseID != "" && receipt.ResponseID != record.ResponseID) {
				return errors.New("persisted provider receipt does not belong to its request")
			}
			if receipt.SupersedesReceiptID != "" {
				if i == 0 || receipt.SupersedesReceiptID != record.Receipts[i-1].ReceiptID {
					return errors.New("persisted receipt supersession does not match preceding evidence")
				}
			} else if i > 0 {
				if err := assertUsageNotDecreasing(previousReceiptUsage, receipt.Usage); err != nil {
					return err
				}
			}
			previousReceiptUsage = receipt.Usage
			if receipt.ActualUSD != nil {
				if previousActual != nil && *receipt.ActualUSD < *previousActual && receipt.SupersedesReceiptID == "" {
					return errors.New("persisted absolute receipt cost decreased")
				}
				previousActual = receipt.ActualUSD
			}
		}
		if (len(record.Receipts) > 0) != (record.Status == UsageRequestReconciled) || (record.ActualUSD != nil && len(record.Receipts) == 0) || (record.Status == UsageRequestReconciled && !record.UsageReported) {
			return errors.New("persisted authoritative provider cost lacks receipt evidence")
		}
		if (previousActual == nil) != (record.ActualUSD == nil) || (previousActual != nil && *previousActual != *record.ActualUSD) {
			return errors.New("persisted actual provider cost disagrees with its receipt evidence")
		}
		if len(record.Receipts) > 0 && previousReceiptUsage != record.Usage {
			return errors.New("persisted reconciled tokens disagree with the last imported receipt")
		}
	}
	for _, item := range run.Items {
		for _, attempt := range item.Usage {
			if !validUsageUSD(attempt.SpentUSD) || !validUsageUSD(attempt.LegacySpentUSD) || !validUsageUSD(attempt.RequestSpentUSD) {
				return errors.New("invalid persisted worker attempt spend")
			}
			key := item.ID + "\x00" + attempt.Worker + "\x00" + attempt.Token
			if math.Abs(requestTotals[key]-attempt.RequestSpentUSD) > 1e-12 {
				return errors.New("persisted provider request total disagrees with the worker attempt ledger")
			}
			// A legacy attempt written before this schema may only have
			// SpentUSD. New-format attempts must agree with both ledgers.
			if (attempt.LegacySpentUSD != 0 || attempt.RequestSpentUSD != 0) && math.Abs(attempt.SpentUSD-math.Max(attempt.LegacySpentUSD, attempt.RequestSpentUSD)) > 1e-12 {
				return errors.New("persisted combined worker spend disagrees with its ledgers")
			}
		}
	}
	return nil
}
