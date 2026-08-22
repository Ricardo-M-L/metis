package webui

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

// The export button writes the same glyph-led transcript the CLI /export
// command produces, into METIS_HOME/exports, and reports the path.
func TestExportEndpointWritesTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)

	s, store := testServer(t)
	opened := ""
	s.openPath = func(path string) error {
		opened = path
		return nil
	}
	h := s.handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/sessions", bytes.NewBufferString(`{"model":"test-model"}`)))
	if rr.Code != 201 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created sessionItem
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(created.ID, llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "help me build a CI pipeline"}},
	}); err != nil {
		t.Fatal(err)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/export",
		bytes.NewBufferString(`{"sessionId":"`+created.ID+`"}`)))
	if rr.Code != 200 {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Path, filepath.Join(home, "exports")) {
		t.Fatalf("export path %q should live under METIS_HOME/exports", out.Path)
	}
	data, err := os.ReadFile(out.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("CI pipeline")) {
		t.Fatalf("transcript should contain the user message:\n%s", data)
	}
	if fi, err := os.Stat(out.Path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("export file should be 0600, got %v", fi.Mode().Perm())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/exports/open", nil))
	if rr.Code != 200 {
		t.Fatalf("open exports directory: %d %s", rr.Code, rr.Body.String())
	}
	if want := filepath.Join(home, "exports"); opened != want {
		t.Fatalf("opened exports directory = %q, want %q", opened, want)
	}

	// Bad session ids must be rejected before any store access.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/export", bytes.NewBufferString(`{"sessionId":"../x"}`)))
	if rr.Code != 400 {
		t.Fatalf("bad session id: %d", rr.Code)
	}
}

// The trajectory export writes a readable tree into METIS_HOME/exports.
func TestTraceExportEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)

	adapter := rtpkg.InstallTrace(t.TempDir())
	if adapter == nil {
		t.Fatal("InstallTrace returned nil")
	}
	defer func() {
		if st := rtpkg.CurrentTraceStore(); st != nil {
			_ = st.Close()
		}
	}()
	adapter.SetSession("sess-x")
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "working on it"})
	adapter.OnEvent(agent.Event{Kind: agent.EventToolStart, ToolName: "Bash", ToolUseID: "t1"})
	adapter.OnEvent(agent.Event{
		Kind: agent.EventToolResult, ToolName: "Bash", ToolUseID: "t1",
		ToolResult: &agent.ToolResult{IsError: true},
	})
	rtpkg.FlushTrace()

	s, _ := testServer(t)
	h := s.handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/trace/export",
		bytes.NewBufferString(`{"sessionId":"sess-x"}`)))
	if rr.Code != 200 {
		t.Fatalf("trace export: %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tool_start Bash", "tool_result Bash", "[ERROR]", "text"} {
		if !bytes.Contains(data, []byte(want)) {
			t.Fatalf("trace export missing %q:\n%s", want, data)
		}
	}

	// Unknown session: 404.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/trace/export",
		bytes.NewBufferString(`{"sessionId":"no-such-session"}`)))
	if rr.Code != 404 {
		t.Fatalf("unknown session: %d", rr.Code)
	}

	// GET is no longer accepted for the side-effecting export.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/trace/export?sessionId=sess-x", nil))
	if rr.Code != 405 {
		t.Fatalf("GET trace export should 405, got %d", rr.Code)
	}

	// Cross-site browser fetches are rejected on GET too (DNS rebinding).
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("cross-site GET should 403, got %d", rr.Code)
	}
}
