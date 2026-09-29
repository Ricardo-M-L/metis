package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestDesktopPreferencesPersistAcrossServers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	s, _ := testServer(t)

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/preferences", nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"busyEnter":"queue"`)) {
		t.Fatalf("defaults: %d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/preferences",
		bytes.NewBufferString(`{"busyEnter":"send","sidebarView":"flat","sidebarSort":"manual","sessionOrder":["s2","s1"],"defaultPreset":"plan","language":"en","rootTurnParallelism":12,"totalAgentParallelism":24,"subagentParallelism":10}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	info, err := os.Stat(filepath.Join(home, "desktop-preferences.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("preferences mode = %o", info.Mode().Perm())
	}

	other, _ := testServer(t)
	rr = httptest.NewRecorder()
	other.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/preferences", nil))
	var got desktopPreferences
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.BusyEnter != "send" || got.SidebarView != "flat" || got.SidebarSort != "manual" || got.DefaultPreset != "plan" || got.Language != "en" || got.RootTurnParallelism != 12 || got.TotalAgentParallelism != 24 || got.SubagentParallelism != 10 || len(got.SessionOrder) != 2 {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestDesktopPreferencesRejectInvalidValueWithoutOverwrite(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/preferences",
		bytes.NewBufferString(`{"busyEnter":"discard"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid preference status = %d: %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/preferences", nil))
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"busyEnter":"queue"`)) {
		t.Fatalf("invalid write changed defaults: %s", rr.Body.String())
	}
}

func TestDesktopPreferencesRejectInvalidParallelismWithoutOverwrite(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, _ := testServer(t)
	for _, body := range []string{`{"rootTurnParallelism":13}`, `{"totalAgentParallelism":0}`, `{"totalAgentParallelism":65}`, `{"subagentParallelism":0}`, `{"subagentParallelism":33}`} {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/preferences", bytes.NewBufferString(body)))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid parallelism %s status = %d: %s", body, rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/preferences", nil))
	var got desktopPreferences
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RootTurnParallelism != DefaultDesktopRootTurnParallelism || got.TotalAgentParallelism != DefaultDesktopTotalAgentParallelism || got.SubagentParallelism != DefaultDesktopSubagentParallelism {
		t.Fatalf("invalid parallelism changed defaults: %+v", got)
	}
}

type resizablePreferenceRunner struct {
	total   int
	perRoot int
}

func (r *resizablePreferenceRunner) Eligible(*session.Header) bool { return true }

func (r *resizablePreferenceRunner) Run(context.Context, IsolatedTurnRequest) (IsolatedTurnResult, error) {
	return IsolatedTurnResult{}, nil
}

func (r *resizablePreferenceRunner) SetDesktopAgentLimits(total, perRoot int) {
	r.total, r.perRoot = total, perRoot
}

func TestDesktopPreferencesApplyParallelismWhenIdle(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	runner := &resizablePreferenceRunner{}
	s, _ := testServer(t)
	s.isolatedRunner = runner
	s.turnCoordinator = NewTurnCoordinator(8)
	s.maxTurnParallelism = MaxDesktopRootTurnParallelism
	s.maxTotalAgentSlots = DefaultDesktopTotalAgentParallelism
	s.maxSubagentsPerRoot = DefaultDesktopSubagentParallelism

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/preferences",
		bytes.NewBufferString(`{"rootTurnParallelism":12,"totalAgentParallelism":24,"subagentParallelism":10}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("save parallelism: %d %s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"parallelismApplied":true`)) {
		t.Fatalf("parallelism response = %s", rr.Body.String())
	}
	if got := s.turnCoordinator.MaxParallel(); got != 12 {
		t.Fatalf("server max parallel = %d, want 12", got)
	}
	if runner.total != 24 || runner.perRoot != 10 {
		t.Fatalf("agent limits = total %d per-root %d, want 24 and 10", runner.total, runner.perRoot)
	}

	lease, err := s.turnCoordinator.Acquire(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/preferences",
		bytes.NewBufferString(`{"rootTurnParallelism":10,"totalAgentParallelism":20,"subagentParallelism":6}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"parallelismApplied":false`)) {
		t.Fatalf("active parallelism response = %d %s", rr.Code, rr.Body.String())
	}
	if got := s.turnCoordinator.MaxParallel(); got != 12 || runner.total != 24 || runner.perRoot != 10 {
		t.Fatalf("live work should defer resize: parallel=%d total=%d per-root=%d", got, runner.total, runner.perRoot)
	}
	lease.Release()
	// A later child-only change may apply the saved total while idle, but it
	// must not silently change the effective root limit from the busy request.
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/preferences",
		bytes.NewBufferString(`{"subagentParallelism":7}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"parallelismApplied":true`)) || s.turnCoordinator.MaxParallel() != 12 || runner.total != 20 || runner.perRoot != 7 {
		t.Fatalf("idle replay response=%d %s, root=%d total=%d per-root=%d", rr.Code, rr.Body.String(), s.turnCoordinator.MaxParallel(), runner.total, runner.perRoot)
	}
}

func TestDesktopPreferencesTotalEditPreservesRootEnvironmentOverride(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	runner := &resizablePreferenceRunner{}
	s, _ := testServer(t)
	s.isolatedRunner = runner
	s.turnCoordinator = NewTurnCoordinator(12) // Startup environment override.
	s.maxTurnParallelism = MaxDesktopRootTurnParallelism
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/preferences", bytes.NewBufferString(`{"totalAgentParallelism":24}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"parallelismApplied":true`)) {
		t.Fatalf("total edit = %d %s", rr.Code, rr.Body.String())
	}
	if got := s.turnCoordinator.MaxParallel(); got != 12 || runner.total != 24 || runner.perRoot != DefaultDesktopSubagentParallelism {
		t.Fatalf("total edit changed effective root limit: root=%d total=%d per-root=%d", got, runner.total, runner.perRoot)
	}
}

func TestDesktopPreferencesDeferWhileDetachedAgentRuns(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	slotDir := t.TempDir()
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOT_DIR", slotDir)
	t.Setenv("METIS_DESKTOP_SUBAGENT_SLOTS", "16")
	t.Setenv("METIS_DESKTOP_SUBAGENT_CAP", "8")
	runner := &resizablePreferenceRunner{}
	s, _ := testServer(t)
	s.isolatedRunner = runner
	s.turnCoordinator = NewTurnCoordinator(8)
	s.maxTurnParallelism = MaxDesktopRootTurnParallelism
	s.desktopAgentSlotDir = slotDir

	release, err := agent.AcquireDesktopSubagentSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/preferences", bytes.NewBufferString(`{"totalAgentParallelism":24}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"parallelismApplied":false`)) {
		t.Fatalf("detached agent should defer resize: %d %s", rr.Code, rr.Body.String())
	}
	if runner.total != 0 || s.turnCoordinator.MaxParallel() != 8 {
		t.Fatalf("detached agent changed live limits: total=%d roots=%d", runner.total, s.turnCoordinator.MaxParallel())
	}
	release()

	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/preferences", bytes.NewBufferString(`{"subagentParallelism":7}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"parallelismApplied":true`)) || runner.total != 24 || runner.perRoot != 7 {
		t.Fatalf("idle change should apply saved total: %d %s, total=%d per-root=%d", rr.Code, rr.Body.String(), runner.total, runner.perRoot)
	}
}

func TestDesktopPreferencesRejectDuplicateManualOrder(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/preferences",
		bytes.NewBufferString(`{"sessionOrder":["same","same"]}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("duplicate order status = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSteerEndpointUsesActiveRunningSession(t *testing.T) {
	storeServer, store := testServer(t)
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	s := NewServer("127.0.0.1:0", loop, store)
	s.stateMu.Lock()
	s.activeSessionID = "busy-session"
	s.stateMu.Unlock()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.cancelMu.Lock()
	s.cancelTurn = cancel
	s.cancelMu.Unlock()

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/steer",
		bytes.NewBufferString(`{"input":"include the regression result"}`)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("steer: %d %s", rr.Code, rr.Body.String())
	}
	if got := loop.SteerInjectDrainForTest(); got != "include the regression result" {
		t.Fatalf("steer buffer = %q", got)
	}

	// A server without an in-flight cancel handle must reject instead of
	// accepting text that no Run can ever consume.
	storeServer.stateMu.Lock()
	storeServer.activeSessionID = "busy-session"
	storeServer.stateMu.Unlock()
	rr = httptest.NewRecorder()
	storeServer.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/steer",
		bytes.NewBufferString(`{"sessionId":"busy-session","input":"lost"}`)))
	if rr.Code != http.StatusServiceUnavailable && rr.Code != http.StatusConflict {
		t.Fatalf("idle steer status = %d: %s", rr.Code, rr.Body.String())
	}
}
