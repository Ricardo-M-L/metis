package webui

import (
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerPermissionSettingPersistsModeAndFreshDefault(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	loop := agent.NewLoop(&activationTestProvider{name: "wire", model: "model"}, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "system", 2)
	s := NewServer("127.0.0.1:0", loop, store)
	id := "worker-permission-mode"
	wd := t.TempDir()
	if err := s.store.WriteHeaderFull(session.Header{ID: id, Mode: "acceptEdits", WorkDir: wd, Provider: "wire", Model: "model", System: "system"}); err != nil {
		t.Fatal(err)
	}
	s.stateMu.Lock()
	s.activeSessionID = id
	s.activeWorkDir = wd
	s.activeProviderName = "wire"
	s.activeModel = "model"
	s.stateMu.Unlock()
	if err := s.applyPermissionMode(permission.ModeAcceptEdits); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"changes":[{"key":"permission.mode","value":"plan"}]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", rr.Code, rr.Body.String())
	}
	h, _, err := s.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if h.Mode != "plan" || h.PrePlanMode != "acceptEdits" {
		t.Fatalf("worker resume will use stale policy: %+v", h)
	}
	s.stateMu.RLock()
	fresh := s.freshPermissionMode
	s.stateMu.RUnlock()
	if fresh != permission.ModePlan {
		t.Fatalf("new session mode: %s", fresh)
	}
}

func TestWorkerPermissionSettingRejectsDuringWorkspaceLease(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	loop := agent.NewLoop(&activationTestProvider{name: "wire", model: "model"}, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "system", 2)
	s := NewServer("127.0.0.1:0", loop, store)
	wd := t.TempDir()
	s.stateMu.Lock()
	s.activeWorkDir = wd
	s.stateMu.Unlock()
	lease, ok := s.turnCoordinator.TryAcquire(wd)
	if !ok {
		t.Fatal("lease failed")
	}
	defer lease.Release()
	before := s.loop.Gate.Mode()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"changes":[{"key":"permission.mode","value":"plan"}]}`)))
	if rr.Code != http.StatusConflict || s.loop.Gate.Mode() != before {
		t.Fatalf("running worker permission mutated: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"changes":[{"key":"ui.theme","value":"dark"}]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("unrelated theme blocked: %d %s", rr.Code, rr.Body.String())
	}
}
