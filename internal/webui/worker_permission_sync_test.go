package webui

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"context"
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"os"
	"path/filepath"
)

func TestWorkerPermissionStatusUsesRequestedDurableSession(t *testing.T) {
	s, store := testServer(t)
	for _, h := range []session.Header{{ID: "a", Mode: "plan", PrePlanMode: "fullAccess"}, {ID: "b", Mode: "acceptEdits"}} {
		if err := store.WriteHeaderFull(h); err != nil {
			t.Fatal(err)
		}
	}
	s.stateMu.Lock()
	s.activeSessionID = "a"
	s.freshPermissionMode = permission.ModeDefault
	s.stateMu.Unlock()
	for _, tc := range []struct {
		query, id, mode string
		available       bool
	}{{"?sessionId=a", "a", "plan", true}, {"?sessionId=b", "b", "acceptEdits", true}, {"?sessionId=missing", "missing", "", false}, {"", "", "default", true}} {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/status"+tc.query, nil))
		var got struct {
			ID        string `json:"permissionSessionId"`
			Mode      string `json:"permissionMode"`
			Available bool   `json:"permissionModeAvailable"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.ID != tc.id || got.Mode != tc.mode || got.Available != tc.available {
			t.Fatalf("wrong permission scope: %s", rr.Body.String())
		}
	}
}

func TestWorkerPermissionPreventsStaleParentSaveAndAdoptsBeforeUserChange(t *testing.T) {
	s, store := testServer(t)
	s.loop = agent.NewLoop(&activationTestProvider{name: "wire", model: "m"}, tools.NewRegistry(), permission.New(permission.ModePlan), nil, "system", 2)
	s.loop.SetPrePlanMode("default")
	s.loop.SetPlanMode(true)
	s.stateMu.Lock()
	s.activeSessionID = "a"
	s.stateMu.Unlock()
	if err := store.WriteHeaderFull(session.Header{ID: "a", Mode: "acceptEdits"}); err != nil {
		t.Fatal(err)
	}
	s.markWorkerPermissionOwner("a")
	if err := s.persistActiveSessionState(); err != nil {
		t.Fatal(err)
	}
	h, _, _ := store.Load("a")
	if h.Mode != "acceptEdits" {
		t.Fatalf("parent overwrote worker: %+v", h)
	}
	if err := s.syncWorkerPermissionState("a"); err != nil {
		t.Fatal(err)
	}
	if s.loop.Gate.Mode() != permission.ModeAcceptEdits {
		t.Fatalf("parent did not adopt worker mode: %s", s.loop.Gate.Mode())
	}
	if err := s.applyPermissionMode(permission.ModePlan); err != nil {
		t.Fatal(err)
	}
	if err := s.persistActiveSessionState(); err != nil {
		t.Fatal(err)
	}
	h, _, _ = store.Load("a")
	if h.Mode != "plan" || h.PrePlanMode != "acceptEdits" {
		t.Fatalf("user transition lost worker lineage: %+v", h)
	}
}

func TestWorkerPermissionRejectsLateAndCrossSessionFrames(t *testing.T) {
	s, _ := testServer(t)
	old, current := &isolatedActiveTurn{}, &isolatedActiveTurn{}
	s.workerTurns = map[string]*isolatedActiveTurn{"a": current}
	frame := desktopipc.Status{Permission: &desktopipc.PermissionState{SessionID: "a", Mode: "acceptEdits"}}
	s.acceptWorkerStatus("a", old, frame)
	if _, ok := s.workerSnapshot("a"); ok {
		t.Fatal("late worker replaced current snapshot")
	}
	frame.Permission.SessionID = "b"
	s.acceptWorkerStatus("a", current, frame)
	if _, ok := s.workerSnapshot("a"); ok {
		t.Fatal("cross session frame was accepted")
	}
	frame.Permission.SessionID = "a"
	s.acceptWorkerStatus("a", current, frame)
	if got, ok := s.workerSnapshot("a"); !ok || got.Permission.Mode != "acceptEdits" {
		t.Fatal("current permission frame missing")
	}
}

func TestWorkerPermissionSettingRejectsWrongActiveTarget(t *testing.T) {
	s, _ := testServer(t)
	s.stateMu.Lock()
	s.activeSessionID = "b"
	s.stateMu.Unlock()
	s.loop = agent.NewLoop(&activationTestProvider{name: "wire", model: "m"}, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "system", 2)
	before := s.loop.Gate.Mode()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"sessionId":"a","changes":[{"key":"permission.mode","value":"plan"}]}`)))
	if rr.Code != 409 || s.loop.Gate.Mode() != before {
		t.Fatalf("cross-session permission change: %d %s", rr.Code, rr.Body.String())
	}
}

func TestWorkerPermissionHidesUncommittedHeaderAndOldOwnerSnapshot(t *testing.T) {
	s, store := testServer(t)
	if err := store.WriteHeaderFull(session.Header{ID: "a", Mode: "acceptEdits"}); err != nil {
		t.Fatal(err)
	}
	s.setWorkerSnapshot("a", desktopipc.Status{Permission: &desktopipc.PermissionState{SessionID: "a", Mode: "fullAccess"}})
	owner := &isolatedActiveTurn{}
	s.workerTurns = map[string]*isolatedActiveTurn{"a": owner}
	for _, path := range []string{"/api/status?sessionId=a", "/api/sessions/a"} {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		var data map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &data)
		if data["permissionModeAvailable"] != false {
			t.Fatalf("old-owner mode leaked: %s", rr.Body.String())
		}
	}
	s.acceptWorkerStatus("a", owner, desktopipc.Status{Permission: &desktopipc.PermissionState{SessionID: "a", Mode: "plan", PrePlanMode: "default"}})
	// A different Store may observe a candidate header before its fsync has
	// either committed or rolled back. Only this owner's status can authorize UI.
	if err := store.WriteHeaderFull(session.Header{ID: "a", Mode: "acceptEdits"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/status?sessionId=a", "/api/sessions/a"} {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		var data map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &data)
		if data["permissionMode"] != "plan" {
			t.Fatalf("uncommitted header leaked: %s", rr.Body.String())
		}
		if item, ok := data["session"].(map[string]any); ok && item["mode"] != "plan" {
			t.Fatalf("raw candidate leaked: %s", rr.Body.String())
		}
	}
}

type permissionCountingRunner struct{ calls int }

func (*permissionCountingRunner) Eligible(*session.Header) bool { return true }
func (r *permissionCountingRunner) Run(context.Context, IsolatedTurnRequest) (IsolatedTurnResult, error) {
	r.calls++
	return IsolatedTurnResult{Checkpointed: true}, nil
}

func TestTurnExpectedPermissionModeRejectsBeforeWorkerAdmission(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, store := testServer(t)
	s.loop = agent.NewLoop(&activationTestProvider{name: "wire", model: "m"}, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "system", 2)
	runner := &permissionCountingRunner{}
	s.isolatedRunner = runner
	if err := store.WriteHeaderFull(session.Header{ID: "a", Mode: "plan", PrePlanMode: "default", WorkDir: t.TempDir(), Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/turns", strings.NewReader(`{"sessionId":"a","input":"do work","expectedPermissionMode":"acceptEdits"}`)))
	if rr.Code != 409 || runner.calls != 0 {
		t.Fatalf("stale UI mode executed: %d %s calls=%d", rr.Code, rr.Body.String(), runner.calls)
	}
	h, _, _ := store.LoadHeader("a")
	if h.Status != "idle" {
		t.Fatal("rejected request changed turn status")
	}
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/turns", strings.NewReader(`{"sessionId":"a","input":"plan work","expectedPermissionMode":"plan"}`)))
	if rr.Code != 200 || runner.calls != 1 {
		t.Fatalf("matching mode refused: %d %s calls=%d", rr.Code, rr.Body.String(), runner.calls)
	}
}

func TestPermissionSettingsPersistenceFailureRestoresLiveMode(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, store := testServer(t)
	s.loop = agent.NewLoop(&activationTestProvider{name: "wire", model: "m"}, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "system", 2)
	s.stateMu.Lock()
	s.activeSessionID = "a"
	s.activeWorkDir = t.TempDir()
	s.stateMu.Unlock()
	if err := store.CheckpointPermissionState("a", "acceptEdits", ""); err != nil {
		t.Fatal(err)
	}
	saved := store.Dir + "-saved"
	transitioned := false
	s.loop.Gate.SetModeChangeListener(func(mode permission.Mode) {
		if mode != permission.ModePlan || transitioned {
			return
		}
		transitioned = true
		if err := os.Rename(store.Dir, saved); err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(store.Dir, []byte("block permission commit after live transition"), 0600); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(func() { _ = os.Remove(store.Dir); _ = os.Rename(saved, store.Dir) })
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"sessionId":"a","changes":[{"key":"permission.mode","value":"plan"}]}`)))
	if !transitioned || rr.Code != 500 || s.loop.Gate.Mode() != permission.ModeAcceptEdits || s.loop.PrePlanMode() != "" {
		t.Fatalf("failed permission save accepted: %d %s mode=%s lineage=%s", rr.Code, rr.Body.String(), s.loop.Gate.Mode(), s.loop.PrePlanMode())
	}
	resumed, _ := session.NewStore(saved)
	h, _, err := resumed.LoadHeader("a")
	if err != nil || h.Mode != "acceptEdits" {
		t.Fatalf("durable mode changed after failed save: %+v %v", h, err)
	}
	if _, err := os.Stat(filepath.Join(saved, "a.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestPermissionSettingsRejectsAnotherProcessTurnOwner(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, store := testServer(t)
	s.loop = agent.NewLoop(&activationTestProvider{name: "wire", model: "m"}, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "system", 2)
	s.stateMu.Lock()
	s.activeSessionID = "a"
	s.activeWorkDir = t.TempDir()
	s.stateMu.Unlock()
	if err := store.CheckpointPermissionState("a", "acceptEdits", ""); err != nil {
		t.Fatal(err)
	}
	other, _ := session.NewStore(store.Dir)
	lock, err := other.AcquireTurnLock(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"sessionId":"a","changes":[{"key":"permission.mode","value":"plan"}]}`)))
	if rr.Code != 409 || s.loop.Gate.Mode() != permission.ModeAcceptEdits {
		t.Fatalf("changed another worker's permission: %d %s", rr.Code, rr.Body.String())
	}
}
