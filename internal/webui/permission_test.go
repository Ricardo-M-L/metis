package webui

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// The browser approval cards resolve through POST /api/permission; the
// pending entry is normally created by handleTurn when the agent loop
// surfaces a permission request.
func TestPermissionResolve(t *testing.T) {
	s, _ := testServer(t)
	h := s.handler()

	reply := make(chan agent.PermissionDecision, 1)
	s.pendingPerms["req-1"] = &permissionPending{reply: reply, tool: "Bash"}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/permission",
		bytes.NewBufferString(`{"id":"req-1","approve":true}`)))
	if rr.Code != 200 {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	select {
	case d := <-reply:
		if d != agent.PermissionDecisionAllow {
			t.Fatalf("decision = %v, want allow", d)
		}
	case <-time.After(time.Second):
		t.Fatal("reply channel was not resolved")
	}

	// Resolved requests cannot be resolved twice.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/permission",
		bytes.NewBufferString(`{"id":"req-1","approve":false}`)))
	if rr.Code != 404 {
		t.Fatalf("second resolve should 404, got %d", rr.Code)
	}

	// Malformed bodies are rejected.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/permission",
		bytes.NewBufferString(`{}`)))
	if rr.Code != 400 {
		t.Fatalf("empty body should 400, got %d", rr.Code)
	}
}

// The Configuration tab reads the real config.toml through /api/config/file.
func TestConfigFileEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)

	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/config/file", nil))
	if rr.Code != 200 {
		t.Fatalf("config file: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !bytes.Contains(rr.Body.Bytes(), []byte("userPath")) {
		t.Fatalf("missing userPath: %s", body)
	}
}

func TestConfigFileEndpointRejectsReplacedMetisHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory replacement semantics differ on Windows")
	}
	parent := t.TempDir()
	home := filepath.Join(parent, "metis-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[ui]\nmarkdown = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_HOME", home)
	if _, _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	if err := os.Rename(home, home+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	const replacement = "[provider.openai]\napi_key = \"replacement-secret\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/config/file", nil))
	if rr.Code != 500 {
		t.Fatalf("config file after root replacement: %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "replacement-secret") {
		t.Fatalf("replacement config was exposed: %s", rr.Body.String())
	}
}

// The AskUser queue resolves through POST /api/ask.
func TestAskResolve(t *testing.T) {
	s, _ := testServer(t)
	h := s.handler()

	reply := make(chan string, 1)
	s.pendingAsks["ask-1"] = &askPending{reply: reply}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/ask",
		bytes.NewBufferString(`{"id":"ask-1","answer":"use Go"}`)))
	if rr.Code != 200 {
		t.Fatalf("ask: %d %s", rr.Code, rr.Body.String())
	}
	select {
	case a := <-reply:
		if a != "use Go" {
			t.Fatalf("answer = %q", a)
		}
	case <-time.After(time.Second):
		t.Fatal("ask reply channel was not resolved")
	}

	// Second resolve of the same id 404s.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/ask",
		bytes.NewBufferString(`{"id":"ask-1","answer":"again"}`)))
	if rr.Code != 404 {
		t.Fatalf("second ask should 404, got %d", rr.Code)
	}
}

func TestAskTimeoutResolvesWithoutDeadlock(t *testing.T) {
	s, _ := testServer(t)
	reply := make(chan string, 1)
	s.pendingAsks["ask-timeout"] = &askPending{reply: reply}

	done := make(chan struct{})
	go func() {
		s.timeoutAsk("ask-timeout")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("AskUser timeout deadlocked")
	}
	select {
	case answer := <-reply:
		if answer != "" {
			t.Fatalf("timeout answer = %q, want empty fallback", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("AskUser timeout did not resolve reply channel")
	}

	s.askMu.Lock()
	_, stillPending := s.pendingAsks["ask-timeout"]
	s.askMu.Unlock()
	if stillPending {
		t.Fatal("timed out AskUser entry remains pending")
	}
}

// Fork creates a new session holding the transcript up to the index.
func TestForkEndpoint(t *testing.T) {
	s, store := testServer(t)
	h := s.handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/sessions", bytes.NewBufferString(`{"model":"test-model"}`)))
	var created sessionItem
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"first question", "first answer", "second question"} {
		role := llm.RoleAssistant
		if i%2 == 0 {
			role = llm.RoleUser
		}
		if err := store.AppendMessage(created.ID, llm.Message{Role: role, Content: []llm.ContentBlock{{Type: "text", Text: text}}}); err != nil {
			t.Fatal(err)
		}
	}

	// Branch at index 1 keeps the first two messages.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/fork",
		bytes.NewBufferString(`{"sessionId":"`+created.ID+`","messageIndex":1}`)))
	if rr.Code != 200 {
		t.Fatalf("fork: %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	hdr2, msgs, err := store.Load(out.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("branched history len = %d, want 2", len(msgs))
	}
	// The branch must carry a header of its own (model/system preserved) -
	// a headerless session is invisible to resume.
	if hdr2 == nil || hdr2.ID != out.SessionID {
		t.Fatalf("branched session header wrong: %+v", hdr2)
	}

	// Sidebar branching uses -1 as an explicit "latest message" sentinel.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/fork",
		bytes.NewBufferString(`{"sessionId":"`+created.ID+`","messageIndex":-1}`)))
	if rr.Code != 200 {
		t.Fatalf("fork latest: %d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	_, latestMessages, err := store.Load(out.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(latestMessages) != 3 {
		t.Fatalf("latest branch history len = %d, want 3", len(latestMessages))
	}

	// Out-of-range index is rejected.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/fork",
		bytes.NewBufferString(`{"sessionId":"`+created.ID+`","messageIndex":99}`)))
	if rr.Code != 400 {
		t.Fatalf("bad index should 400, got %d", rr.Code)
	}
}

func TestForkEndpointStartsFreshPermissionLifetime(t *testing.T) {
	s, store := testServer(t)
	s.freshPermissionMode = permission.ModeDefault
	parent := session.Header{
		ID: "fork-permission-parent", Provider: "test", Model: "test-model",
		Mode: string(permission.ModePlan), PrePlanMode: string(permission.ModeBypassPermissions),
		AlwaysAllow: []session.SavedRule{{Tool: "Bash", Match: "*", Verb: int(permission.DecisionAllow), Source: "interactive"}},
	}
	if err := store.WriteHeaderFull(parent); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(parent.ID, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "fork me"}}}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/fork",
		bytes.NewBufferString(`{"sessionId":"`+parent.ID+`","messageIndex":0}`)))
	if rr.Code != 200 {
		t.Fatalf("fork: %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	hdr, _, err := store.Load(out.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Mode != string(permission.ModeDefault) || hdr.PrePlanMode != "" || len(hdr.AlwaysAllow) != 0 {
		t.Fatalf("branch inherited permission lifetime: %+v", hdr)
	}
	if hdr.ForkedFrom == nil || hdr.ForkedFrom.SessionID != parent.ID || hdr.ForkedFrom.MessageCount != 1 {
		t.Fatalf("branch lineage = %+v, want parent %q at message 1", hdr.ForkedFrom, parent.ID)
	}
}
