package webui

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestCheckpointSessionAPIIsolationAndBoundOwner(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	workspace := t.TempDir()
	s, store := testServer(t)
	for _, id := range []string{"checkpoint-a", "checkpoint-b"} {
		if err := store.WriteHeaderFull(session.Header{ID: id, WorkDir: workspace}); err != nil {
			t.Fatal(err)
		}
	}
	h := s.handler()
	post := func(id, command string, extra map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body := map[string]any{"sessionId": id, "command": command}
		for key, value := range extra {
			body[key] = value
		}
		data, _ := json.Marshal(body)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/checkpoint", bytes.NewReader(data)))
		return rr
	}
	rr := post("checkpoint-a", "off", nil)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "/checkpoint") {
		t.Fatalf("switch A: %d %s", rr.Code, rr.Body.String())
	}
	a, b := checkpoint.NewManager("checkpoint-a", workspace, ""), checkpoint.NewManager("checkpoint-b", workspace, "")
	if a.ControlStatus().Enabled || !b.ControlStatus().Enabled {
		t.Error("Desktop switch did not bind solely to session A")
	}
	view := s.checkpointStatusForView("checkpoint-a", workspace)
	encoded, _ := json.Marshal(view)
	var owner map[string]any
	_ = json.Unmarshal(encoded, &owner)
	if view.Enabled || owner["sessionId"] != "checkpoint-a" {
		t.Errorf("status poll did not read actual session A owner: %s", encoded)
	}
	release, _, err := a.BeginRun()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if rr := post("checkpoint-b", "off", nil); rr.Code != http.StatusOK {
		t.Errorf("A running blocked B: %d %s", rr.Code, rr.Body.String())
	}
	if rr := post("checkpoint-a", "on", nil); rr.Code != http.StatusConflict {
		t.Errorf("active A was mutable: %d %s", rr.Code, rr.Body.String())
	}
	if rr := post("", "off", nil); rr.Code != http.StatusBadRequest {
		t.Errorf("unbound draft modified workspace switch: %d %s", rr.Code, rr.Body.String())
	}
	if rr := post("checkpoint-b", "on", map[string]any{"roots": []string{workspace}}); rr.Code != http.StatusBadRequest {
		t.Errorf("removed scope selector was still accepted: %d %s", rr.Code, rr.Body.String())
	}
}

func TestCheckpointFreshStatusIsReadOnlyAndHomeEnableIsSessionLocal(t *testing.T) {
	home, workspace, metisHome := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", metisHome)
	s, store := testServer(t)
	s.selectWorkspacePath(workspace)
	h := s.handler()
	files := func() int {
		t.Helper()
		count := 0
		err := filepath.WalkDir(metisHome, func(_ string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() {
				count++
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := files()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/checkpoint", nil))
	var fresh map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &fresh); err != nil || rr.Code != http.StatusOK || fresh["enabled"] != true || fresh["sessionId"] != "" {
		t.Fatalf("fresh default: %d %s %v", rr.Code, rr.Body.String(), err)
	}
	if after := files(); after != before {
		t.Errorf("default status wrote settings: %d -> %d", before, after)
	}
	if err := store.WriteHeaderFull(session.Header{ID: "checkpoint-home", WorkDir: home}); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"sessionId": "checkpoint-home", "command": "on"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/checkpoint", bytes.NewReader(data)))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"enabled":true`) || strings.Contains(rr.Body.String(), "请在项目工作区中开启") {
		t.Fatalf("home toggle must enable this session without range selection: %d %s", rr.Code, rr.Body.String())
	}
	cli := checkpoint.NewManager("checkpoint-home", home, "")
	other := checkpoint.NewManager("other-home", home, "")
	if !cli.ControlStatus().Enabled || cli.AutomaticDisabled() || !cli.Sparse() || !other.ControlStatus().Enabled || !other.Sparse() {
		t.Fatal("home switch is not session-local or CLI/Desktop disagree")
	}
	if _, err := cli.Command(t.Context(), "off"); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/checkpoint?sessionId=checkpoint-home", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"enabled":false`) {
		t.Fatalf("Desktop did not read CLI off state: %d %s", rr.Code, rr.Body.String())
	}
	if !checkpoint.NewManager("other-home", home, "").ControlStatus().Enabled || checkpoint.NewManager("checkpoint-home", home, "").ControlStatus().Enabled {
		t.Fatal("saved off must survive recreation without disabling another HOME session")
	}
	if _, err := os.Stat(filepath.Join(metisHome, "checkpoints", "checkpoint-home")); !os.IsNotExist(err) {
		t.Errorf("home toggle created a full-home snapshot: %v", err)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/checkpoint/projects?sessionId=checkpoint-home", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("removed project picker route is still exposed: %d %s", rr.Code, rr.Body.String())
	}
}
