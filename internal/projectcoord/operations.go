package projectcoord

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/Ricardo-M-L/metis/pkg/tool"
)

const (
	// FailureOperationUncertain intentionally has no automatic recovery path.
	// A timeout or dead worker says nothing about whether a remote write landed.
	FailureOperationUncertain = "operation_uncertain"
	MaxOperationResultBytes   = 1 << 20
	// These bound the hot/in-flight journal, not lifetime writes. withRun
	// moves completed receipts into durable replay segments before these fill.
	maxOperationRecords      = 1024
	maxOperationResultsBytes = 16 << 20
)

var ErrOperationUncertain = errors.New("project operation is in progress or its outcome is uncertain")

type OperationStatus string

const (
	OperationStarted    OperationStatus = "started"
	OperationCompleted  OperationStatus = "completed"
	OperationUncertain  OperationStatus = "uncertain"
	OperationNotApplied OperationStatus = "not_applied"
)

// OperationInput keeps raw tool arguments out of the operation journal.
// The adapter hashes the canonical JSON arguments before asking to execute.
type OperationInput struct {
	ToolName    string
	InputSHA256 string
	// Occurrence distinguishes deliberate repeated calls within one claim.
	// The adapter resets its per-digest counter on recovery, replaying the
	// corresponding stable slots. Zero means slot one for old callers.
	Occurrence int `json:"occurrence,omitempty"`
}

// OperationRecord is a write-ahead record, not a claim of exactly-once remote
// execution. Only a fully persisted successful ResultJSON can be replayed.
// Uncertain writes require a trusted operator to check the remote outcome.
type OperationRecord struct {
	ID                string          `json:"id"`
	ItemID            string          `json:"item_id"`
	ToolName          string          `json:"tool_name"`
	InputSHA256       string          `json:"input_sha256"`
	Occurrence        int             `json:"occurrence,omitempty"`
	Status            OperationStatus `json:"status"`
	Worker            string          `json:"worker"`
	ClaimToken        string          `json:"claim_token"`
	ExecutionAttempts int             `json:"execution_attempts"`
	StartedAt         time.Time       `json:"started_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	CompletedAt       time.Time       `json:"completed_at,omitempty"`
	ResultJSON        json.RawMessage `json:"result_json,omitempty"`
	Note              string          `json:"note,omitempty"`
	Resolution        string          `json:"resolution,omitempty"`
	ResolutionNote    string          `json:"resolution_note,omitempty"`
	ResolvedAt        time.Time       `json:"resolved_at,omitempty"`
}

type OperationUncertainError struct {
	ItemID      string
	OperationID string
	Status      OperationStatus
}

func (e *OperationUncertainError) Error() string {
	return fmt.Sprintf("work item %q has unresolved operation %q (%s); inspect its outcome before resolving and recovering the item", e.ItemID, e.OperationID, e.Status)
}

func (e *OperationUncertainError) Unwrap() error { return ErrOperationUncertain }

// BeginOperation atomically persists the intent before the adapter executes a
// write. The same run, item, tool and argument hash keep one ID across claims.
// A completed record is returned without executing again. Distinct calls from
// one live claim may run concurrently; changed arguments cannot bypass any
// uncertain write or a prepared write from an earlier claim.
func (s *Store) BeginOperation(workspace, runID, itemID, worker, token string, input OperationInput) (OperationRecord, error) {
	input, err := normalizeOperationInput(input)
	if err != nil {
		return OperationRecord{}, err
	}
	id := operationID(runID, itemID, input)
	updated, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		item, err := run.item(itemID)
		if err != nil {
			return false, err
		}
		if err := item.assertClaim(strings.TrimSpace(worker), token); err != nil {
			// Preserve the fencing error while making the durable safety hold
			// recognizable to the adapter even after reconciliation held the item.
			return false, errors.Join(err, run.assertOperationsResolved(itemID))
		}
		now := s.now()
		if !item.Lease.ExpiresAt.After(now) {
			return false, fmt.Errorf("work item %q preparing claim expired before operation execution", itemID)
		}
		if err := run.assertOperationMayBegin(item, id, now); err != nil {
			return false, err
		}
		for i := range run.Operations {
			op := &run.Operations[i]
			if op.ID != id {
				continue
			}
			if op.Status == OperationCompleted {
				return false, nil
			}
			if op.Status != OperationNotApplied {
				return false, operationUncertain(*op)
			}
			op.Status, op.Worker, op.ClaimToken = OperationStarted, item.Owner, token
			op.Occurrence = input.Occurrence
			op.ExecutionAttempts++
			op.StartedAt, op.UpdatedAt = now, now
			op.CompletedAt, op.ResultJSON, op.Note = time.Time{}, nil, ""
			run.UpdatedAt = now
			run.addEvent(now, "operation_started", itemID, "Writing operation "+id+" was prepared after an explicit not-applied resolution.", item.Owner)
			return true, nil
		}
		if run.OperationArchive != nil {
			if run.archiveLookup == nil {
				return false, errors.New("operation archive reader is unavailable")
			}
			archived, err := run.archiveLookup(run.OperationArchive, id)
			if err != nil {
				return false, err
			}
			if archived != nil {
				return false, nil
			}
		}
		if len(run.Operations) >= maxOperationRecords {
			return false, fmt.Errorf("project run reached its %d unresolved or retryable operation limit; resolve outstanding outcomes before preparing more writes", maxOperationRecords)
		}
		op := OperationRecord{ID: id, ItemID: itemID, ToolName: input.ToolName, InputSHA256: input.InputSHA256, Occurrence: input.Occurrence, Status: OperationStarted, Worker: item.Owner, ClaimToken: token, ExecutionAttempts: 1, StartedAt: now, UpdatedAt: now}
		run.Operations = append(run.Operations, op)
		run.UpdatedAt = now
		run.addEvent(now, "operation_started", itemID, "Writing operation "+id+" was durably prepared.", item.Owner)
		return true, nil
	})
	if err != nil {
		return OperationRecord{}, err
	}
	// withRun performs a second reconciliation before persisting. A lease
	// which expired across that transaction must not authorize actual I/O.
	op, err := updated.operation(id)
	if err != nil {
		return OperationRecord{}, err
	}
	item, err := updated.item(itemID)
	if err != nil {
		return OperationRecord{}, err
	}
	if err := item.assertClaim(strings.TrimSpace(worker), token); err != nil {
		return OperationRecord{}, errors.Join(err, updated.assertOperationsResolved(itemID))
	}
	if !item.Lease.ExpiresAt.After(s.now()) {
		return OperationRecord{}, fmt.Errorf("work item %q preparing claim expired before operation execution: %w", itemID, ErrOperationUncertain)
	}
	if op.Status != OperationStarted && op.Status != OperationCompleted {
		return OperationRecord{}, operationUncertain(*op)
	}
	return cloneOperation(*op), nil
}

// FinishOperation accepts only the exact live claim which prepared the write.
// Failed, malformed or oversized results must be marked uncertain, never
// silently truncated or interpreted as proof that the write did not happen.
func (s *Store) FinishOperation(workspace, runID, itemID, worker, token, id string, resultJSON json.RawMessage) (OperationRecord, error) {
	if err := validateOperationResult(resultJSON); err != nil {
		return OperationRecord{}, err
	}
	updated, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		now := s.now()
		op, item, err := run.claimedOperation(itemID, worker, token, id, now)
		if err != nil {
			return false, err
		}
		if op.Status != OperationStarted {
			return false, fmt.Errorf("operation %q is %s, not started", id, op.Status)
		}
		if err := run.checkOperationResultBudget(id, resultJSON); err != nil {
			return false, err
		}
		op.Status = OperationCompleted
		op.ResultJSON = append(json.RawMessage(nil), resultJSON...)
		op.CompletedAt, op.UpdatedAt, run.UpdatedAt = now, now, now
		run.addEvent(now, "operation_completed", itemID, "Writing operation "+id+" stored its complete successful result for replay.", item.Owner)
		return true, nil
	})
	if err != nil {
		return OperationRecord{}, err
	}
	op, err := updated.operation(id)
	if err != nil {
		return OperationRecord{}, err
	}
	return cloneOperation(*op), nil
}

// MarkOperationUncertain holds the work item after a tool error, cancellation,
// panic or result-persistence failure. The unexpired lease is retained as a
// safety barrier: operator resolution must wait until that worker is fenced.
func (s *Store) MarkOperationUncertain(workspace, runID, itemID, worker, token, id, note string) (OperationRecord, error) {
	if err := validateOperationNote(note); err != nil {
		return OperationRecord{}, err
	}
	updated, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		now := s.now()
		op, item, err := run.claimedOperation(itemID, worker, token, id, now)
		if err != nil {
			return false, err
		}
		if op.Status != OperationStarted {
			return false, fmt.Errorf("operation %q is %s, not started", id, op.Status)
		}
		op.Status, op.Note, op.UpdatedAt = OperationUncertain, strings.TrimSpace(note), now
		run.holdOperationItem(item, *op, now)
		run.UpdatedAt = now
		run.addEvent(now, "operation_uncertain", itemID, "Writing operation "+id+" has no confirmed outcome; the item is held for operator resolution.", strings.TrimSpace(worker))
		return true, nil
	})
	if err != nil {
		return OperationRecord{}, err
	}
	op, err := updated.operation(id)
	if err != nil {
		return OperationRecord{}, err
	}
	return cloneOperation(*op), nil
}

// ResolveOperation belongs only to the trusted coordinator CLI, never the
// worker tool surface. It records external evidence after the old claim is
// fenced. Resolution does not requeue work or expand its attempt budget.
func (s *Store) ResolveOperation(workspace, runID, id, outcome string, resultJSON json.RawMessage, note string) (OperationRecord, error) {
	if err := validateOperationNote(note); err != nil {
		return OperationRecord{}, err
	}
	if outcome != "applied" && outcome != "not_applied" {
		return OperationRecord{}, errors.New("operation resolution must be applied or not_applied")
	}
	if outcome == "applied" {
		if err := validateOperationResult(resultJSON); err != nil {
			return OperationRecord{}, err
		}
	} else if len(resultJSON) != 0 {
		return OperationRecord{}, errors.New("not_applied operation resolution must not supply a result")
	}
	var result OperationRecord
	_, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		op, err := run.operation(id)
		if err != nil {
			return false, err
		}
		item, err := run.item(op.ItemID)
		if err != nil {
			return false, err
		}
		now := s.now()
		if item.Lease != nil && item.Lease.ExpiresAt.After(now) {
			return false, fmt.Errorf("work item %q still has an unexpired worker lease; stop the worker and wait until %s before resolving", item.ID, item.Lease.ExpiresAt.Format(time.RFC3339Nano))
		}
		if op.Status != OperationStarted && op.Status != OperationUncertain {
			return false, fmt.Errorf("operation %q is already resolved (%s)", id, op.Status)
		}
		if outcome == "applied" {
			if err := run.checkOperationResultBudget(id, resultJSON); err != nil {
				return false, err
			}
			op.Status, op.CompletedAt = OperationCompleted, now
			op.ResultJSON = append(json.RawMessage(nil), resultJSON...)
		} else {
			op.Status, op.CompletedAt, op.ResultJSON = OperationNotApplied, time.Time{}, nil
		}
		op.Resolution, op.ResolutionNote, op.ResolvedAt, op.UpdatedAt = outcome, strings.TrimSpace(note), now, now
		item.Owner, item.Lease = "", nil
		run.UpdatedAt = now
		result = cloneOperation(*op)
		run.addEvent(now, "operation_resolved", item.ID, "Operator resolved writing operation "+id+" as "+outcome+"; explicit work recovery is still required.", "")
		return true, nil
	})
	if err != nil {
		return OperationRecord{}, err
	}
	return result, nil
}

func (r *Run) operation(id string) (*OperationRecord, error) {
	for i := range r.Operations {
		if r.Operations[i].ID == id {
			return &r.Operations[i], nil
		}
	}
	if r.OperationArchive != nil {
		if r.archiveLookup == nil {
			return nil, errors.New("operation archive reader is unavailable")
		}
		op, err := r.archiveLookup(r.OperationArchive, id)
		if err != nil {
			return nil, err
		}
		if op != nil {
			return op, nil
		}
	}
	return nil, fmt.Errorf("project operation %q not found", id)
}

func (r *Run) claimedOperation(itemID, worker, token, id string, now time.Time) (*OperationRecord, *WorkItem, error) {
	item, err := r.item(itemID)
	if err != nil {
		return nil, nil, err
	}
	if err := item.assertClaim(strings.TrimSpace(worker), token); err != nil {
		return nil, nil, err
	}
	if !item.Lease.ExpiresAt.After(now) {
		return nil, nil, fmt.Errorf("work item %q operation claim expired", itemID)
	}
	op, err := r.operation(id)
	if err != nil {
		return nil, nil, err
	}
	if op.ItemID != itemID || op.Worker != item.Owner || op.ClaimToken != token {
		return nil, nil, fmt.Errorf("operation %q was prepared by another work item or worker claim", id)
	}
	return op, item, nil
}

func (r *Run) assertOperationsResolved(itemID string) error {
	for _, op := range r.Operations {
		if op.ItemID == itemID && (op.Status == OperationStarted || op.Status == OperationUncertain) {
			return operationUncertain(op)
		}
	}
	return nil
}

func (r *Run) assertOperationMayBegin(item *WorkItem, id string, now time.Time) error {
	for _, op := range r.Operations {
		if op.ItemID != item.ID || (op.Status != OperationStarted && op.Status != OperationUncertain) {
			continue
		}
		// Distinct calls from one active claim can execute concurrently. The
		// exact same call cannot execute twice, and uncertainty from any call
		// prevents a changed argument from bypassing the item's safety hold.
		live := op.Status == OperationStarted && item.Lease != nil && item.Owner == op.Worker && item.Lease.Token == op.ClaimToken && item.Lease.ExpiresAt.After(now)
		if !live || op.ID == id {
			return operationUncertain(op)
		}
	}
	return nil
}

func operationUncertain(op OperationRecord) error {
	return &OperationUncertainError{ItemID: op.ItemID, OperationID: op.ID, Status: op.Status}
}

// reconcileOperations runs before lease recovery. A prepared write survives a
// process crash as uncertain; returning that item to the ready queue would
// wrongly execute a potentially already-applied remote operation again.
func (r *Run) reconcileOperations(now time.Time) bool {
	changed := false
	for i := range r.Operations {
		op := &r.Operations[i]
		if op.Status != OperationStarted && op.Status != OperationUncertain {
			continue
		}
		item, err := r.item(op.ItemID)
		if err != nil {
			continue // validateOperations rejects a corrupt persisted relationship.
		}
		live := item.Status == ItemRunning && item.Lease != nil && item.Owner == op.Worker && item.Lease.Token == op.ClaimToken && item.Lease.ExpiresAt.After(now)
		if op.Status == OperationStarted && live {
			continue
		}
		if op.Status == OperationStarted {
			op.Status, op.UpdatedAt = OperationUncertain, now
			op.Note = "The preparing worker lost its claim before a successful operation result was persisted."
			r.addEvent(now, "operation_uncertain", item.ID, "Writing operation "+op.ID+" lost its preparing worker; automatic recovery is held.", op.Worker)
			changed = true
		}
		if r.holdOperationItem(item, *op, now) {
			changed = true
		}
	}
	if changed {
		r.UpdatedAt = now
	}
	return changed
}

func (r *Run) holdOperationItem(item *WorkItem, op OperationRecord, now time.Time) bool {
	changed := false
	if item.Status != ItemFailed || item.Failure == nil || item.Failure.Code != FailureOperationUncertain {
		item.Status = ItemFailed
		item.Failure = &Failure{Code: FailureOperationUncertain, Summary: "A writing tool operation has no confirmed outcome: " + op.ID, SuggestedAction: "Inspect the external side effect, resolve the operation as applied or not_applied after the lease expires, then explicitly recover this item.", ObservedAt: now}
		item.UpdatedAt = now
		changed = true
	}
	if item.Lease != nil && !item.Lease.ExpiresAt.After(now) {
		item.Lease, item.Owner = nil, ""
		item.UpdatedAt = now
		changed = true
	}
	return changed
}

func normalizeOperationInput(input OperationInput) (OperationInput, error) {
	if input.Occurrence < 0 {
		return OperationInput{}, errors.New("operation occurrence must be positive (or zero for slot one)")
	}
	if input.Occurrence == 0 {
		input.Occurrence = 1
	}
	input.ToolName = strings.TrimSpace(input.ToolName)
	if input.ToolName == "" || len(input.ToolName) > 256 || strings.ContainsFunc(input.ToolName, unicode.IsControl) {
		return OperationInput{}, errors.New("operation tool name must be nonempty, contain no control characters and fit within 256 bytes")
	}
	input.InputSHA256 = strings.ToLower(input.InputSHA256)
	decoded, err := hex.DecodeString(input.InputSHA256)
	if err != nil || len(decoded) != sha256.Size {
		return OperationInput{}, errors.New("operation input_sha256 must be a 64-character SHA-256 hex digest")
	}
	return input, nil
}

func operationID(runID, itemID string, input OperationInput) string {
	// JSON encodes tuple boundaries, so embedded punctuation cannot collide.
	fields := []string{runID, itemID, input.ToolName, input.InputSHA256}
	// Keep the original identity of legacy zero/default slot-one records.
	if input.Occurrence > 1 {
		fields = append(fields, fmt.Sprintf("occurrence:%d", input.Occurrence))
	}
	tuple, _ := json.Marshal(fields)
	sum := sha256.Sum256(tuple)
	return "op-" + hex.EncodeToString(sum[:])
}

func validateOperationResult(result json.RawMessage) error {
	if len(result) == 0 || len(result) > MaxOperationResultBytes {
		return fmt.Errorf("operation result JSON must contain 1..%d bytes", MaxOperationResultBytes)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(result, &fields); err != nil || fields == nil {
		return errors.New("operation result must be a valid JSON object")
	}
	// writeRun pretty-prints the enclosing graph. A compact JSON array can
	// grow substantially there; accepting it and rejecting the expanded form
	// on reopen would corrupt an otherwise successful operation journal.
	depth, inString, escaped := 0, false, false
	for _, char := range result {
		if inString {
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > 64 {
				return errors.New("operation result JSON exceeds the 64-level nesting limit")
			}
		case '}', ']':
			depth--
		}
	}
	persisted, err := json.MarshalIndent(result, "        ", "  ")
	if err != nil || len(persisted) > MaxOperationResultBytes {
		return fmt.Errorf("operation result JSON exceeds %d bytes when persisted", MaxOperationResultBytes)
	}
	// Reject duplicate outer keys: the replay consumer and operator must see
	// the same result rather than relying on a parser's first/last-key policy.
	decoder := json.NewDecoder(bytes.NewReader(result))
	_, _ = decoder.Token()
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil || seen[key.(string)] {
			return errors.New("operation result must not contain duplicate keys")
		}
		seen[key.(string)] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return errors.New("operation result must be a valid JSON object")
		}
	}
	for key, raw := range fields {
		if strings.EqualFold(strings.ReplaceAll(key, "_", ""), "IsError") {
			var failed bool
			if err := json.Unmarshal(raw, &failed); err != nil || string(bytes.TrimSpace(raw)) == "null" {
				return fmt.Errorf("operation result %s must be a boolean", key)
			}
			if failed {
				return errors.New("failed tool result cannot confirm that a writing operation completed; mark it uncertain")
			}
		}
	}
	var typed tool.Result
	resultDecoder := json.NewDecoder(bytes.NewReader(result))
	resultDecoder.DisallowUnknownFields()
	if err := resultDecoder.Decode(&typed); err != nil {
		return fmt.Errorf("operation result cannot be replayed as a complete tool result: %w", err)
	}
	return nil
}

func validateOperationNote(note string) error {
	if strings.TrimSpace(note) == "" || len(note) > 8192 || strings.ContainsRune(note, '\x00') {
		return errors.New("operation note must be nonempty, contain no NUL and fit within 8192 bytes")
	}
	return nil
}

func (r *Run) checkOperationResultBudget(id string, result json.RawMessage) error {
	// Older results were decoded from the expanded graph file. Measure the
	// new result in its persisted form too, otherwise a compact last result
	// could make the graph exceed its aggregate limit only after it is saved.
	persisted, err := json.MarshalIndent(result, "        ", "  ")
	if err != nil {
		return fmt.Errorf("encode operation result for its journal budget: %w", err)
	}
	total := len(persisted)
	for _, op := range r.Operations {
		if op.ID != id {
			total += len(op.ResultJSON)
		}
	}
	if total > maxOperationResultsBytes {
		return fmt.Errorf("project operation results exceed the %d-byte journal limit", maxOperationResultsBytes)
	}
	return nil
}

func validateOperations(run Run) error {
	if len(run.Operations) > maxOperationRecords {
		return errors.New("persisted project operation journal exceeds its record limit")
	}
	seen := make(map[string]bool, len(run.Operations))
	total := 0
	for _, op := range run.Operations {
		input, err := normalizeOperationInput(OperationInput{ToolName: op.ToolName, InputSHA256: op.InputSHA256, Occurrence: op.Occurrence})
		if err != nil || input.ToolName != op.ToolName || input.InputSHA256 != op.InputSHA256 || op.ID != operationID(run.ID, op.ItemID, input) || seen[op.ID] {
			return fmt.Errorf("persisted operation %q has invalid or duplicate identity", op.ID)
		}
		seen[op.ID] = true
		if _, err := run.item(op.ItemID); err != nil || op.Worker == "" || op.ClaimToken == "" || op.ExecutionAttempts < 1 || op.StartedAt.IsZero() || op.UpdatedAt.IsZero() {
			return fmt.Errorf("persisted operation %q has an invalid preparing claim", op.ID)
		}
		switch op.Status {
		case OperationCompleted:
			if err := validateOperationResult(op.ResultJSON); err != nil || op.CompletedAt.IsZero() {
				return fmt.Errorf("persisted completed operation %q has no valid full result", op.ID)
			}
		case OperationStarted, OperationUncertain:
			if len(op.ResultJSON) != 0 || !op.CompletedAt.IsZero() {
				return fmt.Errorf("persisted operation %q has an invalid unresolved state", op.ID)
			}
		case OperationNotApplied:
			if len(op.ResultJSON) != 0 || !op.CompletedAt.IsZero() || op.Resolution != "not_applied" || op.ResolvedAt.IsZero() {
				return fmt.Errorf("persisted operation %q has an invalid not-applied resolution", op.ID)
			}
		default:
			return fmt.Errorf("persisted operation %q has an unknown status", op.ID)
		}
		if op.Resolution != "" && ((op.Resolution != "applied" && op.Resolution != "not_applied") || op.ResolvedAt.IsZero() || validateOperationNote(op.ResolutionNote) != nil) {
			return fmt.Errorf("persisted operation %q has an invalid resolution audit", op.ID)
		}
		total += len(op.ResultJSON)
	}
	if total > maxOperationResultsBytes {
		return errors.New("persisted project operation results exceed their byte limit")
	}
	return nil
}

func cloneOperation(op OperationRecord) OperationRecord {
	op.ResultJSON = append(json.RawMessage(nil), op.ResultJSON...)
	return op
}

func cloneOperations(operations []OperationRecord) []OperationRecord {
	if operations == nil {
		return nil
	}
	result := make([]OperationRecord, len(operations))
	for i := range operations {
		result[i] = cloneOperation(operations[i])
	}
	return result
}
