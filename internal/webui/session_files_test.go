package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles"
)

func TestSessionFilesReadOnlyHTTP(t *testing.T) {
	root := t.TempDir()
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := store.WriteHeaderFull(session.Header{ID: id, WorkDir: root}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "中文 报告.md")
	if err := os.WriteFile(path, []byte("# Report\napi_key=test-preview-secret\n<script>alert(1)</script>"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("a", llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "[报告](<中文 报告.md>)"}}}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store}
	list := httptest.NewRecorder()
	s.handleSessionFiles(list, httptest.NewRequest(http.MethodGet, "/api/session-files?sessionId=a", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list=%d %s", list.Code, list.Body.String())
	}
	var listed struct {
		Files []sessionfiles.File `json:"files"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Files) != 1 || listed.Files[0].Name != "中文 报告.md" {
		t.Fatalf("list=%+v", listed)
	}
	content := httptest.NewRecorder()
	s.handleSessionFileContent(content, httptest.NewRequest(http.MethodGet, "/api/session-files/content?sessionId=a&id="+listed.Files[0].ID, nil))
	if content.Code != http.StatusOK || content.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(content.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content=%d %s %s", content.Code, content.Header(), content.Body.String())
	}
	if strings.Contains(content.Body.String(), "test-preview-secret") || strings.Contains(content.Body.String(), "<script>") {
		t.Fatalf("unsafe response=%s", content.Body.String())
	}
	var preview sessionfiles.Preview
	if err := json.Unmarshal(content.Body.Bytes(), &preview); err != nil || !strings.Contains(preview.Content, "# Report") {
		t.Fatalf("preview=%+v %v", preview, err)
	}
	for _, tt := range []struct {
		method, target string
		status         int
		list           bool
	}{
		{http.MethodPost, "/api/session-files?sessionId=a", http.StatusMethodNotAllowed, true},
		{http.MethodGet, "/api/session-files", http.StatusBadRequest, true},
		{http.MethodGet, "/api/session-files?sessionId=../a", http.StatusBadRequest, true},
		{http.MethodGet, "/api/session-files?sessionId=absent", http.StatusNotFound, true},
		{http.MethodPost, "/api/session-files/content?sessionId=a&id=" + listed.Files[0].ID, http.StatusMethodNotAllowed, false},
		{http.MethodGet, "/api/session-files/content?sessionId=a", http.StatusBadRequest, false},
		{http.MethodGet, "/api/session-files/content?sessionId=b&id=" + listed.Files[0].ID, http.StatusNotFound, false},
		{http.MethodGet, "/api/session-files/content?sessionId=a&id=" + url.QueryEscape(path), http.StatusNotFound, false},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tt.method, tt.target, nil)
		if tt.list {
			s.handleSessionFiles(w, req)
		} else {
			s.handleSessionFileContent(w, req)
		}
		if w.Code != tt.status {
			t.Errorf("%s %s=%d %s;want %d", tt.method, tt.target, w.Code, w.Body.String(), tt.status)
		}
	}
	empty := httptest.NewRecorder()
	s.handleSessionFiles(empty, httptest.NewRequest(http.MethodGet, "/api/session-files?sessionId=b", nil))
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"files":[]`) {
		t.Fatalf("empty=%d %s", empty.Code, empty.Body.String())
	}
}

func TestSessionFilesHTTPUnavailableStore(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.handleSessionFiles(w, httptest.NewRequest(http.MethodGet, "/api/session-files?sessionId=a", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing store=%d", w.Code)
	}
}
