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

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestCheckpointDesktopAndCLIShareWorkspaceControls(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	if err := os.Mkdir(filepath.Join(home, "project"), 0o700); err != nil {
		t.Fatal(err)
	}
	s, store := testServer(t)
	sid := "checkpoint-web-session"
	err := store.WriteHeaderFull(session.Header{ID: sid, WorkDir: home, Model: "test", Provider: "test"})
	if err != nil {
		t.Fatal(err)
	}
	h := s.handler()
	post := func(command string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"sessionId": sid, "command": command})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/checkpoint", bytes.NewReader(body)))
		return rr
	}
	if rr := post("on"); rr.Code != http.StatusBadRequest {
		t.Fatalf("unscoped home: %d %s", rr.Code, rr.Body.String())
	}
	if rr := post("on project"); rr.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rr.Code, rr.Body.String())
	}
	cli := checkpoint.NewManager("cli-session", home, "")
	if cli.DisabledByPolicy() || !cli.ControlStatus().Enabled {
		t.Fatal("Desktop did not enable CLI")
	}
	release, _, err := cli.BeginRun()
	if err != nil {
		t.Fatal(err)
	}
	if rr := post("off"); rr.Code != http.StatusConflict {
		release()
		t.Fatalf("changed active worker: %d %s", rr.Code, rr.Body.String())
	}
	release()
	if _, err := cli.Command(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/checkpoint?sessionId="+sid, nil))
	var status checkpoint.ControlStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil || rr.Code != http.StatusOK || status.Enabled {
		t.Fatalf("CLI off not reflected: %+v %d %v", status, rr.Code, err)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId="+sid, nil))
	var payload struct {
		Checkpoint checkpoint.ControlStatus `json:"checkpoint"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil || payload.Checkpoint.Enabled || payload.Checkpoint.Mode != "off" {
		t.Fatalf("status HUD: %s %v", rr.Body.String(), err)
	}
	if rr := post(`on "missing project"`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid root: %d", rr.Code)
	}
}
