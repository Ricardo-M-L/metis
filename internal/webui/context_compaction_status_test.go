package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestStatusContextKeepsCommittedUsageDuringSavingAndUpdatesAfterCommit(t *testing.T) {
	provider := &composerSummaryProvider{}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model, loop.ContextWindow = provider.ModelID(), provider.MaxContextTokens()
	loop.Compactor = agent.NewCompactor(agent.DefaultCompactionConfig(), loop.Model, loop.ContextWindow, provider)
	for range 15 {
		loop.AppendUser(strings.Repeat("old context ", 100))
	}
	before := loop.ContextDisplaySnapshot().Used
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{InitialSessionID: "display-session"})
	s.contextMu.Lock()
	s.contextSessionID = "display-session"
	s.contextMu.Unlock()
	read := func() map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		var result map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := loop.CompactNow(context.Background(), agent.CompactOptions{Force: true,
			Persist: func([]llm.Message) error { close(started); <-release; return nil }})
		done <- err
	}()
	<-started
	if got := read(); got["contextUsed"] != float64(before) || got["contextCompactionStage"] != "saving" || got["contextSessionId"] != "display-session" {
		t.Errorf("candidate leaked through Desktop status: %#v", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := read(); got["contextUsed"] != float64(loop.DisplayContextTokens()) || got["contextUsed"].(float64) >= float64(before) || got["contextCompactionStage"] != "" {
		t.Errorf("committed Desktop status=%#v", got)
	}
}

func TestStatusWorkerContextPreservesCompactionDisplayStage(t *testing.T) {
	s, _ := testServer(t)
	s.contextMu.Lock()
	s.contextSessionID = "worker-display"
	s.contextMu.Unlock()
	s.setWorkerSnapshot("worker-display", desktopipc.Status{Context: &desktopipc.ContextPressure{Used: 96000, Window: 128000, CompactionStage: "saving"}})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId=worker-display", nil))
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["contextUsed"] != float64(96000) || payload["contextCompactionStage"] != "saving" {
		t.Fatalf("worker stage lost: %#v", payload)
	}
}
