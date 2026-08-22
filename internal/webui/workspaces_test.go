package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func workspaceRequest(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
	return rr
}

func TestWorkspaceRegistryLifecyclePersistsWithoutDeletingSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}

	base, store := testServer(t)
	opened := ""
	s := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{
		OpenWorkspace: func(path string) error { opened = path; return nil },
	})
	h := s.handler()

	add := func(path, name string) workspaceView {
		body, _ := json.Marshal(map[string]string{"path": path, "name": name})
		rr := workspaceRequest(t, h, http.MethodPost, "/api/workspaces", string(body))
		if rr.Code != http.StatusCreated {
			t.Fatalf("add %s: %d %s", path, rr.Code, rr.Body.String())
		}
		var view workspaceView
		if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	firstView := add(first, "Alpha")
	secondView := add(second, "Beta")
	if firstView.ID == "" || secondView.ID == "" || firstView.ID == secondView.ID {
		t.Fatalf("unstable workspace ids: %+v %+v", firstView, secondView)
	}

	if err := store.WriteHeaderFull(session.Header{ID: "workspace-session", WorkDir: second, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("workspace-session", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "keep"}}}); err != nil {
		t.Fatal(err)
	}

	renameBody, _ := json.Marshal(map[string]string{"id": secondView.ID, "name": "Renamed Beta"})
	rr := workspaceRequest(t, h, http.MethodPost, "/api/workspaces/rename", string(renameBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rr.Code, rr.Body.String())
	}
	reorderBody, _ := json.Marshal(map[string]any{"ids": []string{secondView.ID, firstView.ID}})
	rr = workspaceRequest(t, h, http.MethodPost, "/api/workspaces/reorder", string(reorderBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("reorder: %d %s", rr.Code, rr.Body.String())
	}

	rr = workspaceRequest(t, h, http.MethodGet, "/api/workspaces", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var listed struct {
		Workspaces []workspaceView `json:"workspaces"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	positions := map[string]int{}
	for i, view := range listed.Workspaces {
		positions[view.ID] = i
		if view.ID == secondView.ID && view.Name != "Renamed Beta" {
			t.Fatalf("renamed workspace = %+v", view)
		}
	}
	if positions[secondView.ID] >= positions[firstView.ID] {
		t.Fatalf("reorder not preserved: %+v", listed.Workspaces)
	}

	openBody, _ := json.Marshal(map[string]string{"id": firstView.ID})
	rr = workspaceRequest(t, h, http.MethodPost, "/api/workspaces/open", string(openBody))
	if rr.Code != http.StatusAccepted || opened != workspacePathKey(first) {
		t.Fatalf("open: %d path=%q body=%s", rr.Code, opened, rr.Body.String())
	}

	removeBody, _ := json.Marshal(map[string]string{"id": secondView.ID})
	rr = workspaceRequest(t, h, http.MethodPost, "/api/workspaces/remove", string(removeBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", rr.Code, rr.Body.String())
	}
	rr = workspaceRequest(t, h, http.MethodGet, "/api/workspaces", "")
	if bytes.Contains(rr.Body.Bytes(), []byte(secondView.ID)) {
		t.Fatalf("removed workspace still listed: %s", rr.Body.String())
	}
	if _, messages, err := store.Load("workspace-session"); err != nil || len(messages) != 1 {
		t.Fatalf("workspace removal changed session: messages=%d err=%v", len(messages), err)
	}

	info, err := os.Stat(filepath.Join(home, "workspaces.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("workspace registry mode = %o", info.Mode().Perm())
	}

	// A fresh Server sees the same registry: persistence is global rather
	// than tied to the random loopback port used by the Wails window.
	rr = workspaceRequest(t, base.handler(), http.MethodGet, "/api/workspaces", "")
	if !bytes.Contains(rr.Body.Bytes(), []byte(firstView.ID)) || bytes.Contains(rr.Body.Bytes(), []byte(secondView.ID)) {
		t.Fatalf("registry did not persist across servers: %s", rr.Body.String())
	}
}

func TestWorkspaceRegistryValidatesPathsAndLauncher(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, _ := testServer(t)
	h := s.handler()
	rr := workspaceRequest(t, h, http.MethodPost, "/api/workspaces", `{"path":"/definitely/not/a/metis/workspace"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing path status = %d: %s", rr.Code, rr.Body.String())
	}

	dir := t.TempDir()
	body, _ := json.Marshal(map[string]string{"path": dir})
	rr = workspaceRequest(t, h, http.MethodPost, "/api/workspaces", string(body))
	if rr.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rr.Code, rr.Body.String())
	}
	var view workspaceView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	openBody, _ := json.Marshal(map[string]string{"id": view.ID})
	rr = workspaceRequest(t, h, http.MethodPost, "/api/workspaces/open", string(openBody))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing launcher status = %d: %s", rr.Code, rr.Body.String())
	}
}
