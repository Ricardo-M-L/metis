package projectcoord

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func completeArchiveOperation(t *testing.T, f *operationFixture, input OperationInput, result json.RawMessage) OperationRecord {
	t.Helper()
	op := f.begin(t, input)
	completed, err := f.store.FinishOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, op.ID, result)
	if err != nil {
		t.Fatal(err)
	}
	return completed
}

func TestOperationArchiveSustainsMoreThan1024WritesAndReplaysAcrossClaims(t *testing.T) {
	f := newOperationFixture(t, 2)
	input := operationTestInput("repeated deliberate long-running operation")
	const count = maxOperationRecords + 17
	for i := 1; i <= count; i++ {
		input.Occurrence = i
		completeArchiveOperation(t, f, input, json.RawMessage(fmt.Sprintf(`{"Output":"receipt-%d"}`, i)))
	}
	run := f.get(t)
	if run.OperationArchive == nil || run.OperationArchive.Segments < 2 || run.OperationArchive.Records+uint64(len(run.Operations)) != count || len(run.Operations) >= operationArchiveRecords {
		t.Fatalf("receipts were not rotated into bounded segments: archive=%+v hot=%d", run.OperationArchive, len(run.Operations))
	}
	path, _ := f.store.runPath(run.Workspace, run.ID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`receipt-1"`)) || len(data) > 128<<10 {
		t.Fatalf("main run still contains unbounded history: bytes=%d", len(data))
	}
	usage, err := f.store.StorageUsage(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.HotOperations+int(usage.ArchivedOperations) != count || usage.ArchiveBytes == 0 || usage.PendingOperations != 0 {
		t.Fatalf("incorrect storage counters: %+v", usage)
	}
	if _, err := f.store.Fail(run.Workspace, run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, Failure{Code: FailureWorkerInterrupted, Summary: "restart after many durable writes"}, ""); err != nil {
		t.Fatal(err)
	}
	f.store = NewStore(f.store.Root(), withClock(func() time.Time { return f.now }))
	f.item, err = f.store.ClaimNext(run.Workspace, run.ID, ClaimOptions{Worker: "writer", Lease: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{1, operationArchiveRecords, maxOperationRecords, count} {
		input.Occurrence = i
		op := f.begin(t, input)
		if op.Status != OperationCompleted || op.ExecutionAttempts != 1 || !sameOperationJSON(op.ResultJSON, json.RawMessage(fmt.Sprintf(`{"Output":"receipt-%d"}`, i))) {
			t.Fatalf("archived replay changed operation %d: %+v", i, op)
		}
		queried, err := f.store.GetOperation(run.Workspace, run.ID, op.ID)
		if err != nil || !sameOperationJSON(queried.ResultJSON, op.ResultJSON) {
			t.Fatalf("history lookup failed: %+v %v", queried, err)
		}
	}
	all, err := f.store.ListOperations(run.Workspace, run.ID)
	if err != nil || len(all) != count {
		t.Fatalf("complete management history: %d records %v", len(all), err)
	}
	input.Occurrence = count + 1
	unknown := f.begin(t, input)
	f.now = f.now.Add(2 * time.Second)
	after := f.get(t)
	if len(after.Operations) == 0 || itemByID(t, after, f.item.ID).Failure.Code != FailureOperationUncertain {
		t.Fatal("archival bypassed unknown-outcome safety hold")
	}
	if got, err := f.store.GetOperation(run.Workspace, run.ID, unknown.ID); err != nil || got.Status != OperationUncertain {
		t.Fatalf("unknown operation was lost: %+v %v", got, err)
	}
	input.Occurrence++
	if _, err := f.store.BeginOperation(run.Workspace, run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, input); !errors.Is(err, ErrOperationUncertain) {
		t.Fatalf("old worker bypassed unresolved operation after rotation: %v", err)
	}
}

func TestOperationArchiveRotatesResultsBeyond16MiB(t *testing.T) {
	f := newOperationFixture(t, 2)
	result := json.RawMessage(`{"Output":"` + strings.Repeat("x", 900<<10) + `"}`)
	for i := 0; i < 20; i++ {
		completeArchiveOperation(t, f, operationTestInput(fmt.Sprintf("large receipt %d", i)), result)
	}
	run := f.get(t)
	usage, err := f.store.StorageUsage(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.ArchivedResultBytes+uint64(usage.HotResultBytes) <= maxOperationResultsBytes || usage.HotResultBytes >= operationArchiveResults+MaxOperationResultBytes {
		t.Fatalf("result rotation failed: %+v", usage)
	}
	first, err := f.store.GetOperation(run.Workspace, run.ID, operationID(run.ID, f.item.ID, operationTestInput("large receipt 0")))
	if err != nil || !sameOperationJSON(first.ResultJSON, result) {
		t.Fatalf("large historical result was truncated or missing: %v", err)
	}
}

func archiveTestRecords(t *testing.T, f *operationFixture, count int, status OperationStatus) []OperationRecord {
	t.Helper()
	records := make([]OperationRecord, count)
	for i := range records {
		input := operationTestInput(fmt.Sprintf("archive fixture %d", i))
		records[i] = OperationRecord{ID: operationID(f.run.ID, f.item.ID, input), ItemID: f.item.ID, ToolName: input.ToolName, InputSHA256: input.InputSHA256, Occurrence: 1, Status: status, Worker: f.item.Owner, ClaimToken: f.item.Lease.Token, ExecutionAttempts: 1, StartedAt: f.now, UpdatedAt: f.now}
		if status == OperationCompleted {
			records[i].CompletedAt, records[i].ResultJSON = f.now, json.RawMessage(`{"Output":"archived receipt"}`)
		}
	}
	return records
}

func TestOperationArchiveUncommittedSegmentIsIgnoredAfterCrash(t *testing.T) {
	f := newOperationFixture(t, 2)
	original := completeArchiveOperation(t, f, operationTestInput("committed hot receipt"), json.RawMessage(`{"Output":"committed"}`))
	staged := f.get(t)
	staged.Operations = archiveTestRecords(t, f, operationArchiveRecords, OperationCompleted)
	if changed, err := f.store.compactOperations(staged.Workspace, &staged); err != nil || !changed {
		t.Fatalf("stage archive: %v", err)
	}
	// Simulate death after fsync of the segment and before run.json's commit.
	reopened := NewStore(f.store.Root(), withClock(func() time.Time { return f.now }))
	run, err := reopened.Get(f.run.Workspace, f.run.ID)
	if err != nil || run.OperationArchive != nil || len(run.Operations) != 1 || run.Operations[0].ID != original.ID {
		t.Fatalf("orphan segment changed committed state: %+v %v", run.OperationArchive, err)
	}
	f.store = reopened
	op := f.begin(t, operationTestInput("archive fixture 0"))
	if op.Status != OperationStarted {
		t.Fatal("uncommitted replay receipt falsely suppressed a new write")
	}
}

func TestOperationArchiveCommittedSegmentDamageFailsClosed(t *testing.T) {
	for _, damage := range []string{"missing", "index", "result"} {
		t.Run(damage, func(t *testing.T) {
			f := newOperationFixture(t, 2)
			run := f.get(t)
			run.Operations = archiveTestRecords(t, f, operationArchiveRecords, OperationCompleted)
			if err := f.store.writeRun(run.Workspace, run, false); err != nil {
				t.Fatal(err)
			}
			run = f.get(t)
			path, _ := f.store.operationArchivePath(run.Workspace, run.ID, 1)
			switch damage {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "index", "result":
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				at := len(operationArchiveMagic) + 9 + 8
				if damage == "result" {
					at = len(data) - 4
				}
				data[at] ^= 1
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.store.BeginOperation(run.Workspace, run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, operationTestInput("archive fixture 511")); err == nil {
				t.Fatal("damaged replay evidence authorized another physical write")
			}
		})
	}
}

func TestOperationArchiveCapacityWarningKeepsUnresolvedIntents(t *testing.T) {
	f := newOperationFixture(t, 2)
	run := f.get(t)
	run.Operations = archiveTestRecords(t, f, maxOperationRecords, OperationStarted)
	if err := f.store.writeRun(run.Workspace, run, false); err != nil {
		t.Fatal(err)
	}
	usage, err := f.store.StorageUsage(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.PendingOperations != maxOperationRecords || usage.ArchivedOperations != 0 || len(usage.Warnings) == 0 {
		t.Fatalf("pending outcomes were discarded or capacity pressure hidden: %+v", usage)
	}
	if _, err := f.store.BeginOperation(run.Workspace, run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, operationTestInput("cannot exceed concurrent write capacity")); err == nil {
		t.Fatal("unbounded concurrent unresolved writes were admitted")
	}
}

func TestOperationArchivePreventsReleasingAnExecutedClaim(t *testing.T) {
	f := newOperationFixture(t, 2)
	run := f.get(t)
	run.Operations = archiveTestRecords(t, f, operationArchiveRecords, OperationCompleted)
	if err := f.store.writeRun(run.Workspace, run, false); err != nil {
		t.Fatal(err)
	}
	run = f.get(t)
	if len(run.Operations) != 0 || run.OperationArchive == nil {
		t.Fatal("fixture did not archive all successful writes")
	}
	if _, err := f.store.ReleaseUnstartedClaim(run.Workspace, run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token); err == nil {
		t.Fatal("archived side effects were mistaken for an unstarted claim")
	}
	item := itemByID(t, f.get(t), f.item.ID)
	if item.Attempts != f.item.Attempts || item.Lease == nil || item.Lease.Token != f.item.Lease.Token {
		t.Fatal("rejected claim release altered its execution history")
	}
}

func TestOperationArchiveMetadataEscapingCannotMakeCommittedReceiptUnreadable(t *testing.T) {
	f := newOperationFixture(t, 2)
	run := f.get(t)
	run.Operations = archiveTestRecords(t, f, operationArchiveRecords, OperationCompleted)
	op := &run.Operations[0]
	op.ResultJSON = json.RawMessage(`{"Output":"` + strings.Repeat("x", MaxOperationResultBytes-(10<<10)) + `"}`)
	op.Note, op.ResolutionNote = strings.Repeat("<", 8192), strings.Repeat("<", 8192)
	op.Resolution, op.ResolvedAt = "applied", f.now
	if err := validateOperations(run); err != nil {
		t.Fatalf("fixture must be a legitimate hot journal: %v", err)
	}
	encoded, err := json.Marshal(op)
	if err != nil || len(encoded) <= MaxOperationResultBytes+(32<<10) {
		t.Fatal("fixture did not exceed the old record metadata allowance")
	}
	expectedID := op.ID
	if err := f.store.writeRun(run.Workspace, run, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Get(run.Workspace, run.ID); err != nil {
		t.Fatalf("rotate legal hot receipt: %v", err)
	}
	got, err := f.store.GetOperation(run.Workspace, run.ID, expectedID)
	if err != nil || got.ResolutionNote != strings.Repeat("<", 8192) || !sameOperationJSON(got.ResultJSON, op.ResultJSON) {
		t.Fatalf("committed archive receipt became unreadable: %v", err)
	}
}

func TestOperationArchiveConcurrentStoresRotateOnceAndPrepareOneWrite(t *testing.T) {
	f := newOperationFixture(t, 2)
	run := f.get(t)
	run.Operations = archiveTestRecords(t, f, operationArchiveRecords, OperationCompleted)
	if err := f.store.writeRun(run.Workspace, run, false); err != nil {
		t.Fatal(err)
	}
	input := operationTestInput("two processes encounter the rotation boundary")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store := NewStore(f.store.Root(), withClock(func() time.Time { return f.now }))
			_, err := store.BeginOperation(run.Workspace, run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, input)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes, held := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrOperationUncertain) {
			held++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || held != 1 {
		t.Fatalf("rotation authorized duplicate writes: success=%d held=%d", successes, held)
	}
	after := f.get(t)
	if after.OperationArchive == nil || after.OperationArchive.Segments != 1 || after.OperationArchive.Records != operationArchiveRecords || len(after.Operations) != 1 {
		t.Fatalf("concurrent rotation lost or duplicated receipts: %+v hot=%d", after.OperationArchive, len(after.Operations))
	}
}

func TestOperationArchiveRejectsUnreadableLegacyMetadataBeforeCommit(t *testing.T) {
	f := newOperationFixture(t, 2)
	run := f.get(t)
	run.Operations = archiveTestRecords(t, f, operationArchiveRecords, OperationCompleted)
	// Legacy versions did not bound worker names. Refuse to commit a record
	// outside the reader's bound, even if that metadata already exists on disk.
	run.Operations[0].Worker = strings.Repeat("w", maxOperationArchiveRecordBytes)
	if err := f.store.writeRun(run.Workspace, run, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Get(run.Workspace, run.ID); err == nil {
		t.Fatal("oversized legacy metadata was committed to an unreadable archive")
	}
	path, _ := f.store.runPath(run.Workspace, run.ID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk Run
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.OperationArchive != nil || len(disk.Operations) != operationArchiveRecords {
		t.Fatal("failed archive replaced the original hot journal")
	}
	if _, err := f.store.ClaimNext(run.Workspace, run.ID, ClaimOptions{Worker: strings.Repeat("w", 257)}); err == nil {
		t.Fatal("new claims still admit unbounded worker metadata")
	}
}

func TestOperationArchiveRotationPreservesUsageAndEventDedup(t *testing.T) {
	f := newOperationFixture(t, 2)
	input := EventInput{Source: "archive-test", ID: "external-1", Subject: "Do once", Prompt: "Do once"}
	first, err := f.store.EnqueueEvent(f.run.Workspace, f.run.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	run := f.get(t)
	run.Operations = archiveTestRecords(t, f, operationArchiveRecords, OperationCompleted)
	// Non-ledger historical spend is supported by the existing contract.
	run.SpentUSD = 1.25
	run.Items[0].SpentUSD = 1.25
	before, _ := json.Marshal(run.UsageRequests)
	if err := f.store.writeRun(run.Workspace, run, false); err != nil {
		t.Fatal(err)
	}
	run = f.get(t)
	after, _ := json.Marshal(run.UsageRequests)
	if run.SpentUSD != 1.25 || run.Items[0].SpentUSD != 1.25 || !bytes.Equal(before, after) {
		t.Fatal("operation rotation changed accounting")
	}
	second, err := f.store.EnqueueEvent(run.Workspace, run.ID, input)
	if err != nil || second.ID != first.ID || len(f.get(t).Items) != 3 {
		t.Fatalf("operation rotation lost external event dedup: %+v %v", second, err)
	}
}
