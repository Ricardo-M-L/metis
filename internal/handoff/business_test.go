package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func businessFixture(t *testing.T) (*Store, Record, BusinessUpdate) {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := s.Send(context.Background(), SendRequest{Scope: "jobs", From: "requester", To: "job:worker", Body: "write the report", Task: &Task{ID: "task", Objective: "produce report", Deliverables: []string{"reports/result.json"}, Acceptance: []string{"valid JSON"}}})
	if err != nil {
		t.Fatal(err)
	}
	u := BusinessUpdate{Assignment: BusinessAssignment{JobID: "worker", Workspace: "/workspace", RunID: "run-1", WorkItemID: "work-1", VerificationItemID: "verify-1", ConfigSHA256: strings.Repeat("c", 64)}, SourceRevision: 0, State: BusinessAccepted}
	return s, r, u
}
func completedUpdate(u BusinessUpdate) BusinessUpdate {
	u.SourceRevision++
	u.State = BusinessCompleted
	u.Output = "report produced and independently verified"
	u.OutputSHA256 = digestString(u.Output)
	u.Artifacts = []Artifact{{Path: "reports/result.json", SHA256: strings.Repeat("a", 64)}}
	u.Acceptance = &BusinessAcceptance{Passed: true, Source: "host", EvidenceSHA256: strings.Repeat("e", 64), Summary: "declared output was checked", Checks: []BusinessCheck{{Command: "host:artifact-sha256", ExitCode: 0, OutputSHA256: strings.Repeat("b", 64)}}}
	return u
}
func TestBusinessAckRemainsOnlyTransportCheckpoint(t *testing.T) {
	s, r, u := businessFixture(t)
	claim, err := s.Claim(context.Background(), r.Scope, r.To, "consumer", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Ack(context.Background(), r.ID, claim.Token, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "acknowledged" || got.Business != nil {
		t.Fatalf("transport ack manufactured completion: %#v", got)
	}
	if _, err = s.ReconcileBusiness(context.Background(), r.ID, u); !errors.Is(err, ErrBusinessConflict) {
		t.Fatalf("already delivered message reassigned: %v", err)
	}
}
func TestBusinessLifecycleAndHostEvidence(t *testing.T) {
	s, r, u := businessFixture(t)
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	u.SourceRevision++
	u.State = BusinessRunning
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	ackAssignedBusiness(t, s, r, u.Assignment)
	done := completedUpdate(u)
	for _, change := range []func(*BusinessUpdate){func(x *BusinessUpdate) { x.Acceptance = nil }, func(x *BusinessUpdate) { x.Acceptance.Passed = false }, func(x *BusinessUpdate) { x.Acceptance.Source = "model" }, func(x *BusinessUpdate) { x.Acceptance.EvidenceSHA256 = "" }, func(x *BusinessUpdate) { x.OutputSHA256 = strings.Repeat("f", 64) }, func(x *BusinessUpdate) { x.Artifacts = nil }} {
		bad := done
		a := *done.Acceptance
		bad.Acceptance = &a
		change(&bad)
		if _, err := s.ReconcileBusiness(context.Background(), r.ID, bad); err == nil {
			t.Fatalf("invalid completion accepted: %#v", bad)
		}
	}
	got, err := s.ReconcileBusiness(context.Background(), r.ID, done)
	if err != nil {
		t.Fatal(err)
	}
	if got.Business.State != BusinessCompleted || got.Business.CompletedAt.IsZero() || got.State != "acknowledged" {
		t.Fatalf("incomplete receipt: %#v", got)
	}
	again, err := s.ReconcileBusiness(context.Background(), r.ID, done)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Business.CompletedAt.Equal(got.Business.CompletedAt) {
		t.Fatal("idempotent replay changed receipt time")
	}
	done.SourceRevision++
	done.State = BusinessRunning
	done.Acceptance = nil
	done.Artifacts = nil
	done.Output = ""
	done.OutputSHA256 = ""
	if _, err = s.ReconcileBusiness(context.Background(), r.ID, done); !errors.Is(err, ErrBusinessConflict) {
		t.Fatalf("completed receipt downgraded: %v", err)
	}
}
func TestBusinessStaleConflictRecoveryAndConcurrentIdempotency(t *testing.T) {
	s, r, u := businessFixture(t)
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	u.SourceRevision++
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	stale := u
	stale.SourceRevision = 0
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, stale); err == nil {
		t.Fatal("zero revision accepted")
	}
	conflict := u
	conflict.State = BusinessRunning
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, conflict); !errors.Is(err, ErrBusinessConflict) {
		t.Fatalf("same revision conflict: %v", err)
	}
	conflict = u
	conflict.SourceRevision++
	conflict.Assignment.WorkItemID = "other"
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, conflict); !errors.Is(err, ErrBusinessConflict) {
		t.Fatalf("assignment switched: %v", err)
	}
	u.SourceRevision++
	u.State = BusinessFailed
	u.Failure = &BusinessFailure{Code: "worker_failed", Summary: "execution failed", EvidenceSHA256: strings.Repeat("f", 64)}
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	newer := u
	newer.SourceRevision++
	newer.State = BusinessRunning
	newer.Failure = nil
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, newer); !errors.Is(err, ErrBusinessConflict) {
		t.Fatalf("implicit recovery accepted: %v", err)
	}
	newer.Recovery = true
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); !errors.Is(err, ErrBusinessStale) {
		t.Fatalf("old worker update accepted: %v", err)
	}
	ackAssignedBusiness(t, s, r, u.Assignment)
	done := completedUpdate(newer)
	done.Recovery = false
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := NewStore(s.Root())
			if err != nil {
				t.Error(err)
				return
			}
			if _, err = st.ReconcileBusiness(context.Background(), r.ID, done); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := s.Get(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Business.State != BusinessCompleted || got.Business.SourceRevision != done.SourceRevision {
		t.Fatalf("concurrent result: %#v", got)
	}
	changed := done
	changed.SourceRevision++
	changed.Output = "different"
	changed.OutputSHA256 = digestString(changed.Output)
	if _, err = s.ReconcileBusiness(context.Background(), r.ID, changed); !errors.Is(err, ErrBusinessConflict) {
		t.Fatalf("terminal receipt rewritten: %v", err)
	}
}
func TestBusinessLegacyUpgradeAndManagedDeliverables(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := s.Send(context.Background(), SendRequest{Scope: "s", From: "a", To: "b", Body: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	r.Version = 1
	b, _ := json.Marshal(r)
	p := filepath.Join(s.Root(), "messages", r.ID+".json")
	if err = os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), r.ID)
	if err != nil || got.Business != nil {
		t.Fatalf("legacy read: %#v %v", got, err)
	}
	claim, err := s.Claim(context.Background(), "s", "b", "consumer", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Ack(context.Background(), claim.ID, claim.Token, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(context.Background(), r.ID)
	if err != nil || got.Version != SchemaVersion || got.Version == 1 {
		t.Fatalf("legacy writer protection missing: %#v %v", got, err)
	}
	for _, p := range []string{"../escape", "/absolute", ".", "a/../b", "a//b", "a\\b", "a\nfile"} {
		_, _, err = s.Send(context.Background(), SendRequest{Scope: "s", From: "a", To: "b", Body: "x", Task: &Task{ID: "task", Objective: "write", Deliverables: []string{p}}})
		if err == nil {
			t.Fatalf("unsafe output path accepted %q", p)
		}
	}
}

func ackAssignedBusiness(t *testing.T, s *Store, r Record, a BusinessAssignment) {
	t.Helper()
	c, err := s.ClaimID(context.Background(), r.ID, r.Scope, r.To, "assigned-worker", time.Minute, a)
	if err != nil || c == nil {
		t.Fatalf("assigned claim: %#v %v", c, err)
	}
	if err = s.Ack(context.Background(), r.ID, c.Token, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessReservedClaimFencesGenericAndOldConsumers(t *testing.T) {
	s, r, u := businessFixture(t)
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	generic, err := s.Claim(context.Background(), r.Scope, r.To, "old-generic", time.Second)
	if err != nil || generic == nil {
		t.Fatal(generic, err)
	}
	if _, err = s.ReconcileBusiness(context.Background(), r.ID, u); !errors.Is(err, ErrBusinessConflict) {
		t.Fatalf("active claim was reassigned: %v", err)
	}
	now = now.Add(time.Second)
	if _, err = s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	c, err := s.Claim(context.Background(), r.Scope, r.To, "unrelated", time.Minute)
	if err != nil || c != nil {
		t.Fatalf("generic consumer stole reservation: %#v %v", c, err)
	}
	wrong := u.Assignment
	wrong.RunID = "another-run"
	if _, err = s.ClaimID(context.Background(), r.ID, r.Scope, r.To, "wrong", time.Minute, wrong); !errors.Is(err, ErrBusinessConflict) {
		t.Fatalf("mismatched assignment accepted: %v", err)
	}
	first, err := s.ClaimID(context.Background(), r.ID, r.Scope, r.To, "first-assigned", time.Second, u.Assignment)
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	if err = s.Ack(context.Background(), r.ID, generic.Token, strings.Repeat("a", 64)); !errors.Is(err, ErrLease) {
		t.Fatalf("generic stale ACK accepted: %v", err)
	}
	now = now.Add(time.Second)
	second, err := s.ClaimID(context.Background(), r.ID, r.Scope, r.To, "second-assigned", time.Minute, u.Assignment)
	if err != nil || second == nil || second.Token == first.Token {
		t.Fatal(second, err)
	}
	for _, old := range []string{generic.Token, first.Token} {
		if err = s.Ack(context.Background(), r.ID, old, strings.Repeat("b", 64)); !errors.Is(err, ErrLease) {
			t.Fatal("old ACK", err)
		}
		if err = s.Renew(context.Background(), r.ID, old, time.Minute); !errors.Is(err, ErrLease) {
			t.Fatal("old renew", err)
		}
		if err = s.Release(context.Background(), r.ID, old); !errors.Is(err, ErrLease) {
			t.Fatal("old release", err)
		}
	}
	done := completedUpdate(u)
	if _, err = s.ReconcileBusiness(context.Background(), r.ID, done); err == nil {
		t.Fatal("business completed before durable checkpoint")
	}
	if err = s.Ack(context.Background(), r.ID, second.Token, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	c, err = s.ClaimID(context.Background(), r.ID, r.Scope, r.To, "after-ack", time.Minute, u.Assignment)
	if err != nil || c != nil {
		t.Fatalf("ACK receipt was redelivered: %#v %v", c, err)
	}
	if _, err = s.ReconcileBusiness(context.Background(), r.ID, done); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessConcurrentReservationSingleAssignment(t *testing.T) {
	s, r, u := businessFixture(t)
	const n = 12
	var wg sync.WaitGroup
	successes := make(chan BusinessAssignment, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := NewStore(s.Root())
			if err != nil {
				errs <- err
				return
			}
			update := u
			update.Assignment.RunID = string(rune('a' + i))
			_, err = st.ReconcileBusiness(context.Background(), r.ID, update)
			if err == nil {
				successes <- update.Assignment
			} else if !errors.Is(err, ErrBusinessConflict) {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(successes) != 1 {
		t.Fatalf("reservation winners=%d", len(successes))
	}
	winning := <-successes
	u.Assignment = winning
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal("reservation replay", err)
	}
	reopened, err := NewStore(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(context.Background(), r.ID)
	if err != nil || got.Business.Assignment != winning {
		t.Fatalf("reopen lost binding: %#v %v", got, err)
	}
}

func TestBusinessRejectedExplicitRecoveryAndCancelledTerminal(t *testing.T) {
	for _, state := range []string{BusinessFailed, BusinessBlocked, BusinessRejected, BusinessCancelled} {
		t.Run(state, func(t *testing.T) {
			s, r, u := businessFixture(t)
			if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
				t.Fatal(err)
			}
			u.SourceRevision = 1
			u.State = state
			u.Failure = &BusinessFailure{Code: "host_check_failed", Summary: "invalid artifact", EvidenceSHA256: strings.Repeat("a", 64)}
			if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
				t.Fatal(err)
			}
			next := u
			next.SourceRevision++
			next.State = BusinessAccepted
			next.Failure = nil
			if _, err := s.ReconcileBusiness(context.Background(), r.ID, next); !errors.Is(err, ErrBusinessConflict) {
				t.Fatal("implicit recovery", err)
			}
			next.Recovery = true
			_, err := s.ReconcileBusiness(context.Background(), r.ID, next)
			if state == BusinessCancelled {
				if !errors.Is(err, ErrBusinessConflict) {
					t.Fatal("cancelled task restarted", err)
				}
			} else if err != nil {
				t.Fatal("explicit recovery", err)
			}
		})
	}
}

func TestBusinessSelectorsDoNotStealAutomaticTasks(t *testing.T) {
	s, r, u := businessFixture(t)
	note, _, err := s.Send(context.Background(), SendRequest{Scope: r.Scope, From: "requester", To: r.To, Body: "plain notification"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimNotifications(context.Background(), r.Scope, r.To, "notifications", time.Minute)
	if err != nil || c == nil || c.ID != note.ID {
		t.Fatalf("notification selector: %#v %v", c, err)
	}
	if err = s.Ack(context.Background(), c.ID, c.Token, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	c, err = s.ClaimNotifications(context.Background(), r.Scope, r.To, "notifications", time.Minute)
	if err != nil || c != nil {
		t.Fatalf("structured task stolen: %#v %v", c, err)
	}
	rows, err := s.ListBusiness(context.Background(), r.Scope, r.To, 1)
	if err != nil || len(rows) != 1 || rows[0].ID != r.ID {
		t.Fatalf("pending business: %#v %v", rows, err)
	}
	if _, err = s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	ackAssignedBusiness(t, s, r, u.Assignment)
	rows, err = s.ListBusiness(context.Background(), r.Scope, r.To, 1)
	if err != nil || len(rows) != 1 || rows[0].ID != r.ID || rows[0].State != "acknowledged" {
		t.Fatalf("acknowledged active business disappeared: %#v %v", rows, err)
	}
	done := completedUpdate(u)
	if _, err = s.ReconcileBusiness(context.Background(), r.ID, done); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListBusiness(context.Background(), r.Scope, r.To, 1)
	if err != nil || len(rows) != 0 {
		t.Fatalf("completed receipt still active: %#v %v", rows, err)
	}
	if _, err = s.ListBusiness(context.Background(), "", "", 0); err == nil {
		t.Fatal("unbounded list accepted")
	}
	// Explicit old Claim remains compatible with structured manual job inputs.
	manual, _, err := s.Send(context.Background(), SendRequest{Scope: r.Scope, From: "manual", To: r.To, Body: "manual task", Task: &Task{ID: "manual", Objective: "manual execution"}})
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.Claim(context.Background(), r.Scope, r.To, "manual", time.Minute)
	if err != nil || c == nil || c.ID != manual.ID {
		t.Fatalf("legacy Claim task compatibility: %#v %v", c, err)
	}
}

func TestBusinessSchemaAndEvidenceValidation(t *testing.T) {
	s, r, u := businessFixture(t)
	if err := ValidateEnvelope(r.Envelope); err != nil {
		t.Fatal(err)
	}
	forged := r.Envelope
	forged.Task = &Task{ID: r.Task.ID, Objective: "changed", Deliverables: r.Task.Deliverables}
	if err := ValidateEnvelope(forged); err == nil {
		t.Fatal("content mutation accepted")
	}
	forged = r.Envelope
	forged.Version = 1
	if err := ValidateEnvelope(forged); err == nil {
		t.Fatal("legacy writer could erase deliverables")
	}
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	ackAssignedBusiness(t, s, r, u.Assignment)
	done := completedUpdate(u)
	for _, change := range []func(*BusinessUpdate){
		func(x *BusinessUpdate) { x.Acceptance.Checks = nil },
		func(x *BusinessUpdate) { x.Acceptance.Checks[0].ExitCode = 1 },
		func(x *BusinessUpdate) { x.Acceptance.Checks[0].OutputSHA256 = "" },
		func(x *BusinessUpdate) { x.Artifacts[0].Path = "../escape" },
		func(x *BusinessUpdate) {
			x.Output = strings.Repeat("a", 33<<10)
			x.OutputSHA256 = digestString(x.Output)
		},
	} {
		b, _ := json.Marshal(done)
		var bad BusinessUpdate
		_ = json.Unmarshal(b, &bad)
		change(&bad)
		if _, err := s.ReconcileBusiness(context.Background(), r.ID, bad); err == nil {
			t.Fatalf("invalid evidence accepted: %#v", bad)
		}
	}
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, done); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.Root(), "receipts", r.ID+".json")
	got, err := s.Get(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Version = 1
	b, _ := json.Marshal(got)
	if err = os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(context.Background(), r.ID); err == nil {
		t.Fatal("business metadata on version1 accepted")
	}
}

func TestBusinessAcknowledgedProjectionUsesAuthoritativeReceipt(t *testing.T) {
	s, r, u := businessFixture(t)
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	beforeAck, err := s.Get(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	ackAssignedBusiness(t, s, r, u.Assignment)
	// Simulate a crash after committing a receipt but before removing its old
	// hot inbox record. The newer receipt remains authoritative on restart.
	b, _ := json.Marshal(beforeAck)
	if err = os.WriteFile(filepath.Join(s.Root(), "messages", r.ID+".json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(context.Background(), r.ID)
	if err != nil || got.State != "acknowledged" {
		t.Fatal(got, err)
	}
	done := completedUpdate(u)
	if _, err = reopened.ReconcileBusiness(context.Background(), r.ID, done); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.Root(), "messages", r.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale hot record remained: %v", err)
	}
	got, err = reopened.Get(context.Background(), r.ID)
	if err != nil || got.Business.State != BusinessCompleted {
		t.Fatal(got, err)
	}
}

func TestBusinessCheckCommandMatchesHost8192ByteBoundary(t *testing.T) {
	s, r, u := businessFixture(t)
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	ackAssignedBusiness(t, s, r, u.Assignment)
	done := completedUpdate(u)
	done.Acceptance.Checks[0].Command = strings.Repeat("x", 8193)
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, done); err == nil {
		t.Fatal("8193 byte command accepted")
	}
	done.Acceptance.Checks[0].Command = strings.Repeat("x", 8192)
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, done); err != nil {
		t.Fatal("legal 8192 byte host command cannot complete", err)
	}
}

func TestBusinessReceiptHeadroomPreservesInputLimit(t *testing.T) {
	s, _, u := businessFixture(t)
	paths := make([]string, 64)
	for i := range paths {
		paths[i] = strings.Repeat("p", 1490) + strconv.Itoa(i)
	}
	q := SendRequest{Scope: "jobs", From: "sender", To: "job:worker", Body: strings.Repeat("x", 128<<10), Task: &Task{ID: "large", Objective: "large valid input", Deliverables: paths}}
	r, _, err := s.Send(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Stat(filepath.Join(s.Root(), "messages", r.ID+".json"))
	if err != nil || input.Size() < 220<<10 || input.Size() > maxRecordBytes {
		t.Fatalf("input not near old cap: %v %v", input, err)
	}
	if _, err = s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	ackAssignedBusiness(t, s, r, u.Assignment)
	done := completedUpdate(u)
	done.Output = strings.Repeat("summary ", 4096)
	done.OutputSHA256 = digestString(done.Output)
	done.Artifacts = nil
	for _, p := range paths {
		done.Artifacts = append(done.Artifacts, Artifact{Path: p, SHA256: strings.Repeat("a", 64)})
	}
	done.Acceptance.Checks = make([]BusinessCheck, 64)
	for i := range done.Acceptance.Checks {
		done.Acceptance.Checks[i] = BusinessCheck{Command: strings.Repeat("c", 8192), OutputSHA256: strings.Repeat("b", 64)}
	}
	if _, err = s.ReconcileBusiness(context.Background(), r.ID, done); err != nil {
		t.Fatal("receipt exhausted input-sized cap", err)
	}
	info, err := os.Stat(filepath.Join(s.Root(), "receipts", r.ID+".json"))
	if err != nil || info.Size() <= maxRecordBytes || info.Size() > maxBusinessRecordBytes {
		t.Fatalf("headroom size: %v %v", info, err)
	}
	reopened, err := NewStore(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(context.Background(), r.ID)
	if err != nil || got.Business.State != BusinessCompleted {
		t.Fatal(got, err)
	}
	// Additional headroom is available only to receipt growth, never Send input.
	q.Task.Deliverables = nil
	for i := 0; i < 64; i++ {
		q.Task.Deliverables = append(q.Task.Deliverables, strings.Repeat("x", 4000)+strconv.Itoa(i))
	}
	if _, _, err = s.Send(context.Background(), q); err == nil {
		t.Fatal("plain Send consumed receipt-only headroom")
	}
}

func TestBusinessPaginationAdvancesPastHistoricalFailures(t *testing.T) {
	s, r, u := businessFixture(t)
	now := r.CreatedAt
	s.now = func() time.Time { return now }
	// Each small page models a full production page of old failed/other-run jobs.
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	u.SourceRevision = 1
	u.State = BusinessBlocked
	u.Failure = &BusinessFailure{Code: "old_blocked", Summary: "waiting", EvidenceSHA256: strings.Repeat("b", 64)}
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{r.ID: true}
	for i := 0; i < 4; i++ {
		now = now.Add(time.Second)
		n, _, err := s.Send(context.Background(), SendRequest{Scope: "jobs", From: "sender", To: r.To, Body: "next", Task: &Task{ID: string(rune('a' + i)), Objective: "next"}})
		if err != nil {
			t.Fatal(err)
		}
		want[n.ID] = true
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		rows, next, err := s.ListBusinessPage(context.Background(), "jobs", r.To, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, row := range rows {
			if seen[row.ID] {
				t.Fatal("duplicate page result")
			}
			seen[row.ID] = true
		}
		if next == "" {
			break
		}
		if next == cursor || pages > 5 {
			t.Fatal("cursor did not progress")
		}
		cursor = next
	}
	if len(seen) != len(want) || pages != 3 {
		t.Fatalf("later arrivals starved: seen %d want %d pages %d", len(seen), len(want), pages)
	}
	_, cursor, err := s.ListBusinessPage(context.Background(), "jobs", r.To, "", 1)
	if err != nil || cursor == "" {
		t.Fatal(cursor, err)
	}
	if _, _, err = s.ListBusinessPage(context.Background(), "jobs", "job:another", cursor, 1); err == nil {
		t.Fatal("cursor changed mailboxes")
	}
	if _, _, err = s.ListBusinessPage(context.Background(), "jobs", r.To, "not a cursor", 1); err == nil {
		t.Fatal("malformed cursor accepted")
	}
}

func TestBusinessFailedGraphCanBeExplicitlyCancelled(t *testing.T) {
	s, r, u := businessFixture(t)
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	u.SourceRevision = 1
	u.State = BusinessFailed
	u.Failure = &BusinessFailure{Code: "failure", Summary: "worker failed", EvidenceSHA256: strings.Repeat("a", 64)}
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal(err)
	}
	u.SourceRevision++
	u.State = BusinessCancelled
	u.Failure.Code = "cancelled"
	if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
		t.Fatal("explicit graph cancellation requires no recovery", err)
	}
}

func TestHandoffRecordSizeIncludesTrailingNewline(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	q := SendRequest{Scope: "s", From: "a", To: "b", Body: strings.Repeat("b", 128<<10), Task: &Task{ID: "cap", Objective: "size boundary"}}
	for i := 0; i < 31; i++ {
		q.Task.Acceptance = append(q.Task.Acceptance, strings.Repeat("a", 4096))
	}
	q.Task.Acceptance = append(q.Task.Acceptance, "a")
	template := Record{Envelope: Envelope{Version: SchemaVersion, ID: "msg-" + strings.Repeat("a", 64), Scope: q.Scope, From: q.From, To: q.To, Body: q.Body, Task: q.Task, CreatedAt: now, ContentSHA256: contentSHA(q)}, State: "pending"}
	b, err := json.MarshalIndent(template, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	last := maxRecordBytes - 1 - len(b) + 1
	if last <= 0 || last > 4096 {
		t.Fatal("invalid size fixture", last)
	}
	q.Task.Acceptance[31] = strings.Repeat("a", last)
	exact, _, err := s.Send(context.Background(), q)
	if err != nil {
		t.Fatal("exact inclusive file cap rejected", err)
	}
	st, err := os.Stat(filepath.Join(s.Root(), "messages", exact.ID+".json"))
	if err != nil || st.Size() != maxRecordBytes {
		t.Fatal(st, err)
	}
	if _, err = s.Get(context.Background(), exact.ID); err != nil {
		t.Fatal("successful Send produced unreadable cap-sized record", err)
	}
	q.Task.Acceptance[31] += "a"
	if _, _, err = s.Send(context.Background(), q); err == nil {
		t.Fatal("Send accepted JSON at cap then appended an unreadable extra byte")
	}
}

func TestBusinessRecoveryCanReturnToSameFailureBeforeNextProjection(t *testing.T) {
	for _, state := range []string{BusinessFailed, BusinessBlocked, BusinessRejected} {
		t.Run(state, func(t *testing.T) {
			s, r, u := businessFixture(t)
			if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
				t.Fatal(err)
			}
			u.SourceRevision = 10
			u.State = state
			u.Failure = &BusinessFailure{Code: "first_failure", Summary: "first failure", EvidenceSHA256: strings.Repeat("a", 64)}
			if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); err != nil {
				t.Fatal(err)
			}
			// Explicit graph recovery occurred at 11; the host missed its intermediate
			// running state and next observes an independent new failure at revision 15.
			u.SourceRevision = 15
			u.Failure = &BusinessFailure{Code: "new_failure", Summary: "new attempt failed", EvidenceSHA256: strings.Repeat("b", 64)}
			if _, err := s.ReconcileBusiness(context.Background(), r.ID, u); !errors.Is(err, ErrBusinessConflict) {
				t.Fatalf("same-state outcome overwritten without recovery: %v", err)
			}
			u.Recovery = true
			got, err := s.ReconcileBusiness(context.Background(), r.ID, u)
			if err != nil || got.Business.Failure.Code != "new_failure" {
				t.Fatal(got, err)
			}
		})
	}
}
