package tui

// Verify the bridge HTTP endpoints work end-to-end without requiring
// a live bubbletea session: directly call startBridge and exercise
// each route. Stops the server on each test exit so tests can run
// in parallel without port collisions (each call picks the next
// free port automatically).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBridge_HealthEndpoint(t *testing.T) {
	defer stopBridge()
	addr, err := startBridge(make(chan string, 1))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("health status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "ok" {
		t.Errorf("health body = %q, want 'ok'", string(body))
	}
}

func TestBridge_TranscriptEndpoint(t *testing.T) {
	defer stopBridge()
	addr, err := startBridge(make(chan string, 1))
	if err != nil {
		t.Fatal(err)
	}
	// Push a fake snapshot through the same path the chat surface uses.
	bridgeMu.Lock()
	bridgeSnapshot = bridgeData{
		Messages: []bridgeMessage{
			{Role: "user", Content: "test prompt"},
			{Role: "assistant", Content: "test response"},
		},
		Tokens:    42,
		UpdatedAt: time.Now(),
	}
	bridgeMu.Unlock()

	resp, err := http.Get("http://" + addr + "/transcript")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got bridgeData
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 {
		t.Errorf("messages len = %d, want 2", len(got.Messages))
	}
	if got.Tokens != 42 {
		t.Errorf("tokens = %d, want 42", got.Tokens)
	}
	if got.Messages[0].Content != "test prompt" {
		t.Errorf("first msg = %q", got.Messages[0].Content)
	}
}

func TestBridge_MessagePostQueues(t *testing.T) {
	defer stopBridge()
	ch := make(chan string, 1)
	addr, err := startBridge(ch)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(`{"text":"hello from curl"}`)
	resp, err := http.Post("http://"+addr+"/message", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}
	select {
	case got := <-ch:
		if got != "hello from curl" {
			t.Errorf("queued = %q", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Errorf("nothing queued")
	}
}

func TestBridge_MessageReturnsUnavailableInShareReadOnlyMode(t *testing.T) {
	defer stopBridge()
	addr, err := startBridge(nil)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(`{"text":"must not disappear into an undrained queue"}`)
	resp, err := http.Post("http://"+addr+"/message", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("read-only POST /message status = %d, want 503", resp.StatusCode)
	}
}

func TestBridge_MessageRejectsGet(t *testing.T) {
	defer stopBridge()
	addr, err := startBridge(make(chan string, 1))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/message")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /message status = %d, want 405", resp.StatusCode)
	}
}

func TestBridge_MessageRejectsEmpty(t *testing.T) {
	defer stopBridge()
	addr, err := startBridge(make(chan string, 1))
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(`{"text":""}`)
	resp, err := http.Post("http://"+addr+"/message", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty text status = %d, want 400", resp.StatusCode)
	}
}

func TestBridge_DoubleStartReturnsError(t *testing.T) {
	defer stopBridge()
	if _, err := startBridge(make(chan string, 1)); err != nil {
		t.Fatal(err)
	}
	addr2, err := startBridge(make(chan string, 1))
	if err == nil {
		t.Errorf("second start should error; got addr=%q", addr2)
	}
}

func TestBridge_StopIsIdempotent(t *testing.T) {
	stopBridge()
	stopBridge()
	if cur := bridgeCurrentAddr(); cur != "" {
		t.Errorf("addr should be empty, got %q", cur)
	}
}

func TestBridge_PublishSnapshot(t *testing.T) {
	defer stopBridge()
	if _, err := startBridge(make(chan string, 1)); err != nil {
		t.Fatal(err)
	}
	// Build a minimal Model and call publishBridgeSnapshot. We don't
	// need a working bubbletea instance — just exercise the copy path.
	m := &Model{
		messages:   []Message{{Role: "user", Content: "hi"}},
		toolEvents: []ToolEvent{{ToolName: "Read", Kind: "result"}},
	}
	publishBridgeSnapshot(m)

	bridgeMu.RLock()
	got := bridgeSnapshot
	bridgeMu.RUnlock()
	if len(got.Messages) != 1 {
		t.Errorf("snapshot messages len = %d, want 1", len(got.Messages))
	}
	if len(got.ToolEvents) != 1 {
		t.Errorf("snapshot tool events len = %d, want 1", len(got.ToolEvents))
	}
}
