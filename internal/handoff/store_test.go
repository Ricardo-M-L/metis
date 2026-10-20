package handoff

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSendIdempotencySurvivesReopen(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	q := SendRequest{Scope: "s", From: "main", To: "offline", Body: "inspect build", IdempotencyKey: "call-1", Task: &Task{ID: "task-1", Objective: "Review", Artifacts: []Artifact{{Path: "result.json", SHA256: strings.Repeat("a", 64)}}, Acceptance: []string{"JSON is valid"}}}
	one, dup, err := s.Send(context.Background(), q)
	if err != nil || dup {
		t.Fatalf("send: %v %v", dup, err)
	}
	reopened, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	two, dup, err := reopened.Send(context.Background(), q)
	if err != nil || !dup || one.ID != two.ID {
		t.Fatalf("replay: %#v %v %v", two, dup, err)
	}
	q.Body = "different"
	if _, _, err = reopened.Send(context.Background(), q); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "messages", one.ID+".json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record perms: %v %v", info, err)
	}
}

func TestLeaseCrashRedeliveryFencesOldWorkerAndAck(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	sent, _, err := s.Send(context.Background(), SendRequest{Scope: "s", From: "a", To: "b", Body: "handoff"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Claim(context.Background(), "s", "b", "worker-old", time.Second)
	if err != nil || first == nil {
		t.Fatalf("first: %v %v", first, err)
	}
	other, err := s.Claim(context.Background(), "s", "b", "worker-new", time.Second)
	if err != nil || other != nil {
		t.Fatalf("unexpired claim stolen: %v %v", other, err)
	}
	now = now.Add(time.Second)
	second, err := s.Claim(context.Background(), "s", "b", "worker-new", time.Second)
	if err != nil || second == nil || second.ID != sent.ID || second.Token == first.Token || second.Attempts != 2 {
		t.Fatalf("redelivery: %v %v", second, err)
	}
	if err = s.Ack(context.Background(), sent.ID, first.Token, strings.Repeat("a", 64)); !errors.Is(err, ErrLease) {
		t.Fatalf("stale ack accepted: %v", err)
	}
	if err = s.Renew(context.Background(), sent.ID, first.Token, time.Second); !errors.Is(err, ErrLease) {
		t.Fatalf("stale renew accepted: %v", err)
	}
	if err = s.Release(context.Background(), sent.ID, first.Token); !errors.Is(err, ErrLease) {
		t.Fatalf("stale release accepted: %v", err)
	}
	if err = s.Ack(context.Background(), sent.ID, second.Token, ""); err == nil {
		t.Fatal("ack without checkpoint accepted")
	}
	if err = s.Ack(context.Background(), sent.ID, second.Token, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if err = s.Ack(context.Background(), sent.ID, second.Token, strings.Repeat("b", 64)); err != nil {
		t.Fatalf("duplicate ack: %v", err)
	}
	next, err := s.Claim(context.Background(), "s", "b", "worker-3", time.Second)
	if err != nil || next != nil {
		t.Fatalf("acked redelivered: %v %v", next, err)
	}
}

func TestConcurrentStoresSingleWinner(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	const n = 20
	var wg sync.WaitGroup
	ids := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := NewStore(root)
			if err != nil {
				errs <- err
				return
			}
			r, _, err := st.Send(context.Background(), SendRequest{Scope: "s", From: "a", To: "b", Body: "same", IdempotencyKey: "same"})
			if err != nil {
				errs <- err
				return
			}
			ids <- r.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	id := ""
	for got := range ids {
		if id != "" && id != got {
			t.Fatal("duplicate sends created two IDs")
		}
		id = got
	}
	rows, err := s.List(context.Background(), "s", "b")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows: %v %v", rows, err)
	}
	winners := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, _ := NewStore(root)
			r, err := st.Claim(context.Background(), "s", "b", "worker", time.Minute)
			if err != nil {
				t.Error(err)
				return
			}
			if r != nil {
				winners <- r.ID
			}
		}()
	}
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("claim winners=%d", len(winners))
	}
}

func TestHandoffProcessWorker(t *testing.T) {
	root := os.Getenv("METIS_HANDOFF_PROCESS_TEST_ROOT")
	if root == "" {
		t.Skip("helper subprocess")
	}
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := s.Send(context.Background(), SendRequest{Scope: "s", From: "a", To: "b", Body: "crash survives", IdempotencyKey: "same"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(context.Background(), "s", "b", "process", 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	_ = r // Intentionally exit without acknowledgement: kernel releases flock.
}
func TestIndependentProcessRestart(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHandoffProcessWorker$")
	cmd.Env = append(os.Environ(), "METIS_HANDOFF_PROCESS_TEST_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, out)
	}
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(35 * time.Millisecond)
	r, err := s.Claim(context.Background(), "s", "b", "restarted", time.Second)
	if err != nil || r == nil || r.Attempts != 2 {
		t.Fatalf("restart: %v %v", r, err)
	}
}

func TestMalformedContractAndSymlinksRejected(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []SendRequest{{Scope: "s", From: "a", To: "../b\n", Body: "x"}, {Scope: "s", From: "a", To: "b", Body: "x", Task: &Task{ID: "t", Objective: "x", Artifacts: []Artifact{{Path: "x", SHA256: "bogus"}}}}} {
		if _, _, err := s.Send(context.Background(), q); err == nil {
			t.Fatal("bad envelope accepted")
		}
	}
	bad := filepath.Join(t.TempDir(), "link")
	if err = os.Symlink(root, bad); err == nil {
		if _, err = NewStore(bad); err == nil {
			t.Fatal("symlinked store accepted")
		}
	}
}

func TestSenderNamespacesDoNotCollapseSameNamedPeers(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q := SendRequest{Scope: "jobs", SenderScope: "session:a", From: "alice", To: "job:receiver", Body: "same", IdempotencyKey: "handoff-1"}
	a, _, err := s.Send(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	q.SenderScope = "session:b"
	b, duplicate, err := s.Send(context.Background(), q)
	if err != nil || duplicate || a.ID == b.ID {
		t.Fatalf("independent senders collapsed: %v %v %v", b, duplicate, err)
	}
	q.SenderScope = "session:a"
	again, duplicate, err := s.Send(context.Background(), q)
	if err != nil || !duplicate || again.ID != a.ID {
		t.Fatalf("same sender not idempotent: %v %v %v", again, duplicate, err)
	}
}

func TestStoreDirectoryReplacementFailsClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mail")
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(root, "messages"), filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(root, "messages"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Send(context.Background(), SendRequest{Scope: "s", From: "a", To: "b", Body: "x"}); err == nil {
		t.Fatal("retargeted inbox accepted writes")
	}
}

func TestAcknowledgedReceiptsRetainDedupWithoutLoadingHistoricalQueue(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q := SendRequest{Scope: "s", From: "a", To: "b", Body: "done", IdempotencyKey: "once"}
	r, _, err := s.Send(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(context.Background(), "s", "b", "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Ack(context.Background(), claim.ID, claim.Token, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.Root(), "messages", r.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("acknowledged body remains in hot inbox: %v", err)
	}
	if _, err = os.Stat(filepath.Join(s.Root(), "receipts", r.ID+".json")); err != nil {
		t.Fatal(err)
	}
	again, dup, err := s.Send(context.Background(), q)
	if err != nil || !dup || again.ID != r.ID || again.State != "acknowledged" {
		t.Fatalf("archived dedup=%#v %v %v", again, dup, err)
	}
	// Inspecting corrupt historical receipts remains fail closed, while a
	// new query need not parse unrelated history on every delivery poll.
	if err = os.WriteFile(filepath.Join(s.Root(), "receipts", "corrupt.json"), []byte("not JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	next, err := s.Claim(context.Background(), "s", "b", "worker", time.Minute)
	if err != nil || next != nil {
		t.Fatalf("cold receipts blocked empty inbox: %#v %v", next, err)
	}
	if _, err = s.List(context.Background(), "s", "b"); err == nil {
		t.Fatal("explicit receipt inspection ignored corruption")
	}
}

func TestAcknowledgementBeforeReceiptMoveSurvivesRestart(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q := SendRequest{Scope: "s", From: "a", To: "b", Body: "done", IdempotencyKey: "once"}
	r, _, err := s.Send(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(context.Background(), "s", "b", "old", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Ack(context.Background(), claim.ID, claim.Token, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	// Reproduce the durable on-disk state immediately before receipt rename.
	if err = os.Rename(filepath.Join(s.Root(), "receipts", r.ID+".json"), filepath.Join(s.Root(), "messages", r.ID+".json")); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	next, err := reopened.Claim(context.Background(), "s", "b", "new", time.Minute)
	if err != nil || next != nil {
		t.Fatalf("acknowledged message redelivered: %#v %v", next, err)
	}
	again, dup, err := reopened.Send(context.Background(), q)
	if err != nil || !dup || again.State != "acknowledged" {
		t.Fatalf("migration-gap dedup=%#v %v %v", again, dup, err)
	}
}
