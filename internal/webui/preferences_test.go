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
		bytes.NewBufferString(`{"busyEnter":"send","sidebarView":"flat","sidebarSort":"manual","sessionOrder":["s2","s1"],"defaultPreset":"plan","language":"en"}`)))
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
	if got.BusyEnter != "send" || got.SidebarView != "flat" || got.SidebarSort != "manual" || got.DefaultPreset != "plan" || got.Language != "en" || len(got.SessionOrder) != 2 {
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
