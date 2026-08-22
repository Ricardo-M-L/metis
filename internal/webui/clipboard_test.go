package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/desktop"
)

func TestClipboardFilesEndpointReturnsOnlyMatchingFinderItems(t *testing.T) {
	s, _ := testServer(t)
	s.clipboardFiles = func() ([]desktop.ClipboardFile, error) {
		return []desktop.ClipboardFile{
			{Path: "/Users/test/芯片设计", Name: "芯片设计", IsDir: true},
			{Path: "/Users/test/notes.txt", Name: "notes.txt"},
		}, nil
	}
	body, _ := json.Marshal(map[string]any{"names": []string{"芯片设计"}})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/clipboard/files", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`/Users/test/芯片设计`)) || bytes.Contains(rr.Body.Bytes(), []byte(`notes.txt`)) {
		t.Fatalf("unexpected response: %s", rr.Body.String())
	}
}

func TestClipboardFilesEndpointCanInsertEveryCopiedFinderItemFromComposerMenu(t *testing.T) {
	s, _ := testServer(t)
	s.clipboardFiles = func() ([]desktop.ClipboardFile, error) {
		return []desktop.ClipboardFile{
			{Path: "/Users/test/project", Name: "project", IsDir: true},
			{Path: "/Users/test/spec.md", Name: "spec.md"},
		}, nil
	}
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/clipboard/files", bytes.NewBufferString(`{"all":true}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`/Users/test/project`)) || !bytes.Contains(rr.Body.Bytes(), []byte(`/Users/test/spec.md`)) {
		t.Fatalf("all clipboard files = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestClipboardFilesEndpointRejectsClipboardRaceAndWrongMethod(t *testing.T) {
	s, _ := testServer(t)
	s.clipboardFiles = func() ([]desktop.ClipboardFile, error) {
		return []desktop.ClipboardFile{{Path: "/Users/test/other", Name: "other", IsDir: true}}, nil
	}

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/clipboard/files", bytes.NewBufferString(`{"names":["芯片设计"]}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("clipboard mismatch status = %d, want 409: %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/clipboard/files", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rr.Code)
	}
}
