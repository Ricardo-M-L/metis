package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func resumeStore(t *testing.T) (*Store, string) {
	t.Helper()
	s, id := checkpointStore(t)
	if err := s.AppendMessage(id, historyText(llm.RoleUser, "original request")); err != nil {
		t.Fatal(err)
	}
	return s, id
}

func TestDesktopResumeTokenLifecycleAndRestart(t *testing.T) {
	s, id := resumeStore(t)
	lock, err := s.AcquireTurnLock(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := s.FinishDesktopTurn(id, "stopped", true); err != nil {
		t.Fatal(err)
	}
	hdr, _, err := s.LoadHeader(id)
	if err != nil || !DesktopResumeAvailable(hdr) {
		t.Fatalf("stopped header = %+v, %v", hdr, err)
	}
	token := hdr.ResumeToken
	other, err := NewStore(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.StartDesktopTurn(id, "wrong", true); !errors.Is(err, ErrResumeUnavailable) {
		t.Fatalf("wrong token = %v", err)
	}
	if err := other.StartDesktopTurn(id, token, true); err != nil {
		t.Fatal(err)
	}
	if err := other.StartDesktopTurn(id, token, true); !errors.Is(err, ErrResumeUnavailable) {
		t.Fatalf("reused token = %v", err)
	}
	hdr, history, _ := other.Load(id)
	if hdr.Status != "running" || hdr.ResumeToken != "" || len(history) != 1 {
		t.Fatalf("claim changed history or retained token: %+v, %+v", hdr, history)
	}
	if err := other.FinishDesktopTurn(id, "stopped", true); err != nil {
		t.Fatal(err)
	}
	hdr, _, _ = other.LoadHeader(id)
	if hdr.ResumeToken == token || hdr.ResumeToken == "" {
		t.Fatal("second stop did not rotate token")
	}
	if err := other.StartDesktopTurn(id, "", false); err != nil {
		t.Fatal(err)
	}
	if err := other.StartDesktopTurn(id, hdr.ResumeToken, true); !errors.Is(err, ErrResumeUnavailable) {
		t.Fatalf("new input retained token: %v", err)
	}
	if err := other.FinishDesktopTurn(id, "stopped", false); err != nil {
		t.Fatal(err)
	}
	hdr, _, _ = other.LoadHeader(id)
	if DesktopResumeAvailable(hdr) {
		t.Fatal("force stop offered resume")
	}
}

func TestDesktopResumeTokenIsSessionBound(t *testing.T) {
	s, id := resumeStore(t)
	if err := s.WriteHeaderFull(Header{ID: "other", Model: "model"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage("other", historyText(llm.RoleUser, "B request")); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{id, "other"} {
		if err := s.FinishDesktopTurn(candidate, "stopped", true); err != nil {
			t.Fatal(err)
		}
	}
	a, _, _ := s.LoadHeader(id)
	if err := s.StartDesktopTurn("other", a.ResumeToken, true); !errors.Is(err, ErrResumeUnavailable) {
		t.Fatalf("A token claimed B: %v", err)
	}
}

func TestDesktopResumeRejectsUncheckpointedToolPairs(t *testing.T) {
	s, id := resumeStore(t)
	if err := s.AppendMessage(id, checkpointPair("pending")[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDesktopTurn(id, "stopped", true); err == nil {
		t.Fatal("orphan tool call authorized resume")
	}
	hdr, _, _ := s.LoadHeader(id)
	if DesktopResumeAvailable(hdr) {
		t.Fatal("failed history validation offered resume")
	}
}

func TestDesktopResumeClaimSyncFailureBlocksWork(t *testing.T) {
	s, id := resumeStore(t)
	if err := s.FinishDesktopTurn(id, "stopped", true); err != nil {
		t.Fatal(err)
	}
	hdr, _, _ := s.LoadHeader(id)
	s.syncFile = func(*os.File) error { return errors.New("injected sync failure") }
	if err := s.StartDesktopTurn(id, hdr.ResumeToken, true); err == nil {
		t.Fatal("claim ignored durability failure")
	}
}

func TestDesktopResumeStopSyncFailureDoesNotExposeGrant(t *testing.T) {
	s, id := resumeStore(t)
	s.syncFile = func(*os.File) error { return errors.New("injected sync failure") }
	if err := s.FinishDesktopTurn(id, "stopped", true); err == nil {
		t.Fatal("stop ignored durability failure")
	}
	hdr, _, err := s.LoadHeader(id)
	if err != nil || DesktopResumeAvailable(hdr) || hdr.ResumeToken != "" {
		t.Fatalf("failed grant still visible: %+v, %v", hdr, err)
	}
}

func TestDesktopResumeOrdinaryCLIInputInvalidatesTokenOnly(t *testing.T) {
	s, id := resumeStore(t)
	if err := s.FinishDesktopTurn(id, "stopped", true); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateDesktopResume(id); err != nil {
		t.Fatal(err)
	}
	hdr, history, err := s.Load(id)
	if err != nil || hdr.Status != "stopped" || hdr.ResumeToken != "" || len(history) != 1 {
		t.Fatalf("invalidation changed ordinary status/history: %+v %+v %v", hdr, history, err)
	}
}

func TestDesktopResumeLockProcessHelper(t *testing.T) {
	if os.Getenv("METIS_TEST_RESUME_LOCK") != "1" {
		return
	}
	s, err := NewStore(os.Getenv("METIS_TEST_RESUME_LOCK_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = s.AcquireTurnLock(ctx, "locked")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("child bypassed parent's lock: %v", err)
	}
}

func TestDesktopResumeTurnLockSpansProcesses(t *testing.T) {
	s, _ := checkpointStore(t)
	lock, err := s.AcquireTurnLock(context.Background(), "locked")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDesktopResumeLockProcessHelper$")
	cmd.Env = append(os.Environ(), "METIS_TEST_RESUME_LOCK=1", "METIS_TEST_RESUME_LOCK_DIR="+s.Dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		lock.Release()
		t.Fatalf("child lock: %v, %s", err, output)
	}
	lock.Release()
	other, err := NewStore(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reacquired, err := other.AcquireTurnLock(ctx, "locked")
	if err != nil {
		t.Fatal(err)
	}
	reacquired.Release()
}
