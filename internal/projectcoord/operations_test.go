package projectcoord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type operationFixture struct {
	store *Store
	run   Run
	item  WorkItem
	now   time.Time
}

func newOperationFixture(t *testing.T, attempts int) *operationFixture {
	t.Helper()
	f := &operationFixture{now: time.Now().UTC()}
	f.store = NewStore(filepath.Join(t.TempDir(), "state"), withClock(func() time.Time { return f.now }))
	var err error
	f.run, err = f.store.Create(CreateInput{Workspace: t.TempDir(), Goal: "Persist remote writing operations", Items: []WorkItemInput{{ID: "write", Phase: PhaseCustom, Subject: "Write a remote object", Prompt: "Create one remote object and verify it", MaxAttempts: attempts}}})
	if err != nil {
		t.Fatal(err)
	}
	f.item, err = f.store.ClaimNext(f.run.Workspace, f.run.ID, ClaimOptions{Worker: "writer", Lease: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func operationTestInput(text string) OperationInput {
	sum := sha256.Sum256([]byte(text))
	return OperationInput{ToolName: "remote.write", InputSHA256: hex.EncodeToString(sum[:])}
}

func sameOperationJSON(a, b json.RawMessage) bool {
	var compactA, compactB bytes.Buffer
	return json.Compact(&compactA, a) == nil && json.Compact(&compactB, b) == nil && bytes.Equal(compactA.Bytes(), compactB.Bytes())
}

func (f *operationFixture) begin(t *testing.T, input OperationInput) OperationRecord {
	t.Helper()
	op, err := f.store.BeginOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, input)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func (f *operationFixture) get(t *testing.T) Run {
	t.Helper()
	run, err := f.store.Get(f.run.Workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestOperationCompletedResultIsDurableAndReplayedAcrossClaims(t *testing.T) {
	f := newOperationFixture(t, 2)
	input := operationTestInput(`{"message":"private raw tool arguments"}`)
	op := f.begin(t, input)
	fullResult := json.RawMessage(`{"Output":"created remote-object-123","IsError":false,"Meta":{"remote_id":"remote-object-123"},"Presentation":{"kind":"receipt","id":123},"Images":[]}`)
	finished, err := f.store.FinishOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, op.ID, fullResult)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != OperationCompleted || !sameOperationJSON(finished.ResultJSON, fullResult) {
		t.Fatalf("finished operation = %+v", finished)
	}
	finished.ResultJSON[0] = 'x'
	if _, err := f.store.Fail(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, Failure{Code: FailureWorkerInterrupted, Summary: "Worker stopped after tool completion"}, ""); err != nil {
		t.Fatal(err)
	}
	oldClaim := f.item
	f.item, err = f.store.ClaimNext(f.run.Workspace, f.run.ID, ClaimOptions{Worker: "writer", Lease: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if f.item.Lease.Token == oldClaim.Lease.Token {
		t.Fatal("recovery reused the old claim token")
	}
	replayed := f.begin(t, input)
	if replayed.ID != op.ID || replayed.Status != OperationCompleted || replayed.ExecutionAttempts != 1 || !sameOperationJSON(replayed.ResultJSON, fullResult) {
		t.Fatalf("completed operation was reexecuted or changed on replay: %+v", replayed)
	}
	if _, err := f.store.FinishOperation(f.run.Workspace, f.run.ID, oldClaim.ID, oldClaim.Owner, oldClaim.Lease.Token, op.ID, fullResult); err == nil {
		t.Fatal("old claim finished an operation after the new worker claimed the item")
	}
	reopened, err := NewStore(f.store.Root()).Get(f.run.Workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Operations) != 1 || !sameOperationJSON(reopened.Operations[0].ResultJSON, fullResult) {
		t.Fatalf("reopened operation lost its full result: %+v", reopened.Operations)
	}
	raw, _ := json.Marshal(reopened)
	if bytes.Contains(raw, []byte("private raw tool arguments")) {
		t.Fatal("operation journal retained raw tool arguments")
	}
	cloned := cloneRun(reopened)
	cloned.Operations[0].ResultJSON[0] = 'x'
	if !sameOperationJSON(reopened.Operations[0].ResultJSON, fullResult) {
		t.Fatal("cloning a run aliased its operation result bytes")
	}
}

func TestOperationDuplicateWriteAndUncertainWriteBlockBypassAndCompletion(t *testing.T) {
	f := newOperationFixture(t, 2)
	op := f.begin(t, operationTestInput("first write"))
	_, err := f.store.BeginOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, operationTestInput("first write"))
	if !errors.Is(err, ErrOperationUncertain) {
		t.Fatalf("the same write began twice: %v", err)
	}
	var typed *OperationUncertainError
	if !errors.As(err, &typed) || typed.OperationID != op.ID {
		t.Fatalf("operation hold lost its typed identity: %v", err)
	}
	if _, err := f.store.Complete(context.Background(), f.run.Workspace, f.run.ID, f.item.ID, CompletionInput{Worker: f.item.Owner, Token: f.item.Lease.Token, Output: "All done"}); !errors.Is(err, ErrOperationUncertain) {
		t.Fatalf("work completion hid a prepared write: %v", err)
	}
	if run := f.get(t); len(run.Operations) != 1 || run.Operations[0].Status != OperationStarted || itemByID(t, run, f.item.ID).Status != ItemRunning {
		t.Fatalf("rejected bypass changed the live operation: %+v", run)
	}
	if _, err := f.store.MarkOperationUncertain(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, op.ID, "Remote request returned an ambiguous transport error"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.BeginOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, operationTestInput("changed write")); !errors.Is(err, ErrOperationUncertain) {
		t.Fatalf("changed arguments bypassed the uncertain write safety hold: %v", err)
	}
}

func TestOperationDistinctWritesShareLiveClaimAndAllHoldAfterCrash(t *testing.T) {
	f := newOperationFixture(t, 2)
	first := f.begin(t, operationTestInput("first independent parallel write"))
	second := f.begin(t, operationTestInput("second independent parallel write"))
	if first.ID == second.ID || len(f.get(t).Operations) != 2 {
		t.Fatal("different calls from the live claim did not get independent operations")
	}
	f.now = f.now.Add(2 * time.Second)
	run := f.get(t)
	for _, op := range run.Operations {
		if op.Status != OperationUncertain {
			t.Fatalf("parallel crash did not hold every unconfirmed write: %+v", op)
		}
	}
	if _, err := f.store.ResolveOperation(f.run.Workspace, f.run.ID, first.ID, "not_applied", nil, "The first remote request was not accepted"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Recover(f.run.Workspace, f.run.ID, f.item.ID, "Try after resolving only one call", false); !errors.Is(err, ErrOperationUncertain) {
		t.Fatalf("resolving one parallel call bypassed another uncertain call: %v", err)
	}
	if _, err := f.store.ResolveOperation(f.run.Workspace, f.run.ID, second.ID, "applied", json.RawMessage(`{"Output":"Confirmed second operation receipt"}`), "The second remote receipt confirms it was applied"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Recover(f.run.Workspace, f.run.ID, f.item.ID, "All remote outcomes checked", false); err != nil {
		t.Fatal(err)
	}
}

func TestOperationBeginDoesNotAuthorizeIOAfterPostReconcileLeaseExpiry(t *testing.T) {
	f := newOperationFixture(t, 2)
	beforeExpiry := f.now.Add(100 * time.Millisecond)
	afterExpiry := f.now.Add(2 * time.Second)
	clockCalls := 0
	f.store.now = func() time.Time {
		clockCalls++
		if clockCalls <= 2 { // pre-reconcile, then preparation under the lock
			return beforeExpiry
		}
		return afterExpiry // post-reconcile must fence before actual I/O
	}
	_, err := f.store.BeginOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, operationTestInput("cross-transaction expiry"))
	if !errors.Is(err, ErrOperationUncertain) {
		t.Fatalf("a claim expired during prepare still authorized external execution: %v", err)
	}
	f.now, f.store.now = afterExpiry, func() time.Time { return f.now }
	run := f.get(t)
	if len(run.Operations) != 1 || run.Operations[0].Status != OperationUncertain || itemByID(t, run, f.item.ID).Status != ItemFailed {
		t.Fatalf("post-reconcile did not persist the fenced prepared operation: %+v", run)
	}
}

func TestOperationLeaseExpiryHoldsInsteadOfAutoRecovery(t *testing.T) {
	f := newOperationFixture(t, 2)
	op := f.begin(t, operationTestInput("prepared write"))
	f.now = f.now.Add(2 * time.Second)
	run := f.get(t)
	item := itemByID(t, run, f.item.ID)
	if run.Status != RunBlocked || item.Status != ItemFailed || item.Failure == nil || item.Failure.Code != FailureOperationUncertain || item.Failure.Recoverable || item.Failure.AutoRecovered || item.Lease != nil {
		t.Fatalf("lost prepared write was automatically retried: %+v", item)
	}
	if len(run.Operations) != 1 || run.Operations[0].Status != OperationUncertain || run.Operations[0].ID != op.ID {
		t.Fatalf("lost prepared write was not durably held: %+v", run.Operations)
	}
	if _, err := f.store.ClaimNext(f.run.Workspace, f.run.ID, ClaimOptions{Worker: "other"}); !errors.Is(err, ErrNoReadyWork) {
		t.Fatalf("uncertain operation was claimable: %v", err)
	}
	if _, err := f.store.Recover(f.run.Workspace, f.run.ID, f.item.ID, "Force a retry without checking the write", true); !errors.Is(err, ErrOperationUncertain) {
		t.Fatalf("explicit recovery bypassed unresolved operation: %v", err)
	}
	if _, err := f.store.FinishOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, op.ID, json.RawMessage(`{"Output":"late result"}`)); err == nil {
		t.Fatal("expired worker wrote a late successful operation result")
	}
}

func TestOperationInterruptedWorkerCannotAutoRetryPreparedWrite(t *testing.T) {
	f := newOperationFixture(t, 2)
	f.begin(t, operationTestInput("remote request possibly delivered"))
	item, err := f.store.Fail(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, Failure{Code: FailureWorkerInterrupted, Summary: "Service stopping"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != ItemFailed || item.Failure == nil || item.Failure.Code != FailureOperationUncertain || item.Failure.AutoRecovered {
		t.Fatalf("recoverable worker interruption requeued an unknown write: %+v", item)
	}
}

func TestOperationMarkUncertainAndAppliedResolutionRequireFencedWorker(t *testing.T) {
	f := newOperationFixture(t, 2)
	input := operationTestInput("timed out write")
	op := f.begin(t, input)
	marked, err := f.store.MarkOperationUncertain(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, op.ID, "The remote call timed out after sending its body.")
	if err != nil || marked.Status != OperationUncertain {
		t.Fatalf("mark uncertain = %+v, %v", marked, err)
	}
	item := itemByID(t, f.get(t), f.item.ID)
	if item.Lease == nil || item.Lease.Token != f.item.Lease.Token || item.Status != ItemFailed {
		t.Fatalf("uncertainty discarded the live worker safety barrier: %+v", item)
	}
	result := json.RawMessage(`{"Output":"Operator confirmed message remote-321","IsError":false}`)
	if _, err := f.store.ResolveOperation(f.run.Workspace, f.run.ID, op.ID, "applied", result, "Remote audit log confirms success"); err == nil {
		t.Fatal("operation was resolved while its previous worker lease remained active")
	}
	f.now = f.now.Add(2 * time.Second)
	resolved, err := f.store.ResolveOperation(f.run.Workspace, f.run.ID, op.ID, "applied", result, "Remote audit log confirms success")
	if err != nil || resolved.Status != OperationCompleted || resolved.Resolution != "applied" || !sameOperationJSON(resolved.ResultJSON, result) {
		t.Fatalf("applied resolution = %+v, %v", resolved, err)
	}
	if item := itemByID(t, f.get(t), f.item.ID); item.Status != ItemFailed {
		t.Fatalf("operation resolution automatically requeued its item: %+v", item)
	}
	if _, err := f.store.Recover(f.run.Workspace, f.run.ID, f.item.ID, "Checked external receipt", false); err != nil {
		t.Fatal(err)
	}
	f.item, err = f.store.ClaimNext(f.run.Workspace, f.run.ID, ClaimOptions{Worker: "new writer", Lease: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	replay := f.begin(t, input)
	if replay.Status != OperationCompleted || replay.ExecutionAttempts != 1 || !sameOperationJSON(replay.ResultJSON, result) {
		t.Fatalf("operator-confirmed applied result was not replayed: %+v", replay)
	}
}

func TestOperationNotAppliedResolutionAllowsOnlyExplicitBoundedRecovery(t *testing.T) {
	f := newOperationFixture(t, 1)
	input := operationTestInput("confirmed absent write")
	op := f.begin(t, input)
	f.now = f.now.Add(2 * time.Second)
	resolved, err := f.store.ResolveOperation(f.run.Workspace, f.run.ID, op.ID, "not_applied", nil, "Remote request ledger confirms no request was accepted")
	if err != nil || resolved.Status != OperationNotApplied {
		t.Fatalf("not-applied resolution = %+v, %v", resolved, err)
	}
	if _, err := f.store.Recover(f.run.Workspace, f.run.ID, f.item.ID, "Confirmed absent remotely", false); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("resolution expanded the task attempt budget: %v", err)
	}
	if _, err := f.store.Recover(f.run.Workspace, f.run.ID, f.item.ID, "Use the one explicit retry after confirming absence", true); err != nil {
		t.Fatal(err)
	}
	f.item, err = f.store.ClaimNext(f.run.Workspace, f.run.ID, ClaimOptions{Worker: "writer", Lease: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	retry := f.begin(t, input)
	if retry.ID != op.ID || retry.Status != OperationStarted || retry.ExecutionAttempts != 2 || retry.ClaimToken == op.ClaimToken || retry.ResolutionNote != resolved.ResolutionNote {
		t.Fatalf("explicit not-applied retry lost stable identity or audit: %+v", retry)
	}
	f.now = f.now.Add(2 * time.Second)
	if _, err := f.store.ResolveOperation(f.run.Workspace, f.run.ID, retry.ID, "not_applied", nil, "Remote request ledger confirms the retry also did not land"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Recover(f.run.Workspace, f.run.ID, f.item.ID, "Try a third time", true); err == nil || !strings.Contains(err.Error(), "one forced retry") {
		t.Fatalf("repeated operation resolution enabled an unbounded task retry: %v", err)
	}
}

func TestOperationIndependentStoresBeginOnlyOnce(t *testing.T) {
	f := newOperationFixture(t, 2)
	input := operationTestInput("same concurrent write")
	start := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store := NewStore(f.store.Root(), withClock(func() time.Time { return f.now }))
			<-start
			_, err := store.BeginOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, input)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	started, held := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			started++
		case errors.Is(err, ErrOperationUncertain):
			held++
		default:
			t.Fatalf("concurrent begin failed unexpectedly: %v", err)
		}
	}
	if started != 1 || held != 7 || len(f.get(t).Operations) != 1 {
		t.Fatalf("concurrent writes started=%d held=%d", started, held)
	}
}

func TestOperationResultAndResolutionValidationRejectsAmbiguousSuccess(t *testing.T) {
	for _, result := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`{"Output":`), json.RawMessage(`{"IsError":true}`), json.RawMessage(`{"is_error":true}`), json.RawMessage(`{"ISERROR":true}`), json.RawMessage(`{"IsError":null}`), json.RawMessage(`{"IsError":"false"}`), json.RawMessage(`{"IsError":true,"IsError":false}`), json.RawMessage(`{"Output":123}`), json.RawMessage(`{"Images":"invalid image attachment type"}`), json.RawMessage(`{"Presentation":true}`), json.RawMessage(`{"Unknown":"not a complete tool result field"}`), json.RawMessage(fmt.Sprintf(`{"Output":%q}`, strings.Repeat("x", MaxOperationResultBytes)))} {
		t.Run(fmt.Sprintf("%d-%x", len(result), sha256.Sum256(result)), func(t *testing.T) {
			f := newOperationFixture(t, 2)
			op := f.begin(t, operationTestInput("invalid result"))
			if _, err := f.store.FinishOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, op.ID, result); err == nil {
				t.Fatal("invalid or failed result was recorded as a completed write")
			}
			if got := f.get(t).Operations[0]; got.Status != OperationStarted || len(got.ResultJSON) != 0 {
				t.Fatalf("invalid result modified write certainty: %+v", got)
			}
		})
	}
	f := newOperationFixture(t, 2)
	op := f.begin(t, operationTestInput("invalid resolution"))
	f.now = f.now.Add(2 * time.Second)
	for _, request := range []struct {
		outcome, note string
		result        json.RawMessage
	}{{"applied", "", json.RawMessage(`{"Output":"ok"}`)}, {"unknown", "checked", nil}, {"applied", "checked", nil}, {"not_applied", "checked", json.RawMessage(`{"Output":"ok"}`)}} {
		if _, err := f.store.ResolveOperation(f.run.Workspace, f.run.ID, op.ID, request.outcome, request.result, request.note); err == nil {
			t.Fatalf("invalid resolution was accepted: %+v", request)
		}
	}
	if f.get(t).Operations[0].Status != OperationUncertain {
		t.Fatal("invalid resolution changed the persisted uncertain outcome")
	}
}

func TestOperationJournalValidationDetectsCorruption(t *testing.T) {
	f := newOperationFixture(t, 2)
	f.begin(t, operationTestInput("persisted corruption"))
	original := f.get(t)
	mutations := []func(*Run){
		func(r *Run) { r.Operations[0].ID = "other" },
		func(r *Run) { r.Operations[0].ItemID = "missing" },
		func(r *Run) { r.Operations[0].Status = "unknown" },
		func(r *Run) { r.Operations[0].Status = OperationCompleted },
		func(r *Run) { r.Operations[0].InputSHA256 = "not-a-hash" },
		func(r *Run) { r.Operations[0].ClaimToken = "" },
		func(r *Run) { r.Operations = append(r.Operations, r.Operations[0]) },
	}
	for index, mutate := range mutations {
		run := cloneRun(original)
		mutate(&run)
		if err := validateOperations(run); err == nil {
			t.Fatalf("journal corruption %d was accepted", index)
		}
	}
}

func TestOperationExpandedJSONCannotPoisonPersistedJournal(t *testing.T) {
	for name, result := range map[string]json.RawMessage{
		"indentation": json.RawMessage(`{"Meta":{"values":[` + strings.Repeat("0,", 150000) + `0]}}`),
		"html-escape": json.RawMessage(`{"Output":"` + strings.Repeat("<", 200000) + `"}`),
		"nesting":     json.RawMessage(`{"Meta":` + strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65) + `}`),
	} {
		t.Run(name, func(t *testing.T) {
			if len(result) >= MaxOperationResultBytes || !json.Valid(result) {
				t.Fatal("fixture must be a valid compact result smaller than the encoded result limit")
			}
			f := newOperationFixture(t, 2)
			op := f.begin(t, operationTestInput("large nested tool result"))
			if _, err := f.store.FinishOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, op.ID, result); err == nil {
				t.Fatal("result which expands past its persistence limit was accepted")
			}
			if got := f.get(t).Operations[0]; got.Status != OperationStarted || len(got.ResultJSON) != 0 {
				t.Fatalf("rejecting a large result poisoned the operation journal: %+v", got)
			}
		})
	}
}

func TestOperationAggregateBudgetIncludesNewResultPersistenceExpansion(t *testing.T) {
	result := json.RawMessage(`{"Meta":{"values":[` + strings.Repeat("0,", 60000) + `0]}}`)
	if err := validateOperationResult(result); err != nil {
		t.Fatalf("new result should fit its individual persisted limit: %v", err)
	}
	// Realistically this is the accumulated size of many older persisted
	// results. The aggregate check uses their already-expanded wire bytes.
	oldTotal := maxOperationResultsBytes - 500000
	run := Run{Operations: []OperationRecord{{ID: "old", ResultJSON: bytes.Repeat([]byte(" "), oldTotal)}}}
	if len(result)+oldTotal >= maxOperationResultsBytes {
		t.Fatal("fixture does not exercise compact-size acceptance followed by persisted expansion")
	}
	if err := run.checkOperationResultBudget("new", result); err == nil {
		t.Fatal("new result persistence expansion bypassed the aggregate budget")
	}
	// Replacing this operation subtracts its prior result before adding the
	// new persisted result, so an update is not charged twice.
	if err := run.checkOperationResultBudget("old", result); err != nil {
		t.Fatalf("result replacement incorrectly included its old bytes: %v", err)
	}
}
