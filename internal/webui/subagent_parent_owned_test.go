package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestParentOwnedSubAgentCannotReceiveDirectInput(t *testing.T) {
	for _, kind := range []string{"transcript", "legacy", "unrecorded", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			_, store := testServer(t)
			loop := agent.NewLoop(&incompleteTurnProvider{activationTestProvider: activationTestProvider{name: "test", model: "model"}, stopReason: "end_turn"}, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
			s := NewServer("127.0.0.1:0", loop, store)
			if kind == "transcript" {
				newWebConversationTranscript(t, store.Dir, "child", "parent", nil)
			} else if kind == "legacy" {
				if err := store.WriteHeaderFull(session.Header{ID: "child", SubAgentOf: "parent"}); err != nil {
					t.Fatal(err)
				}
			} else if kind == "unrecorded" {
				s.setWorkerSnapshot("parent", desktopipc.Status{Agents: []desktopipc.Subagent{{AgentID: "child", Status: "queued"}}})
			} else {
				if err := os.MkdirAll(filepath.Join(store.Dir, agent.SubAgentTranscriptDirname), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(store.Dir, agent.SubAgentTranscriptDirname, "child.jsonl"), []byte("bad header\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s.activeSessionID = "child"
			for _, request := range []struct{ path, body string }{
				{"/api/turns", `{"sessionId":"child","input":"continue"}`},
				{"/api/steer", `{"sessionId":"child","input":"continue"}`},
				{"/api/sessions/activate", `{"id":"child"}`},
			} {
				rr := httptest.NewRecorder()
				s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, request.path, strings.NewReader(request.body)))
				if rr.Code != http.StatusForbidden {
					t.Fatalf("%s returned %d: %s", request.path, rr.Code, rr.Body.String())
				}
				var body map[string]any
				if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body["canAcceptDirectInput"] != false || body["ownership"] != "parent" || body["directInputBlockedReason"] != "parent_owned" {
					t.Fatalf("missing ownership capability: %s", rr.Body.String())
				}
			}
		})
	}
}

func TestParentOwnedSubAgentCapabilityAppearsOnReadSurfaces(t *testing.T) {
	s, store := testServer(t)
	newWebConversationTranscript(t, store.Dir, "child", "parent", nil)
	for _, path := range []string{"/api/subagents/child?sessionId=parent", "/api/subagents/child/conversation?sessionId=parent", "/api/subagents?sessionId=parent"} {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s=%d %s", path, rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		detail, ok := payload["agent"].(map[string]any)
		if !ok {
			detail = payload["agents"].([]any)[0].(map[string]any)
		}
		if detail["canAcceptDirectInput"] != false || detail["ownership"] != "parent" || detail["parentSessionId"] != "parent" || detail["directInputBlockedReason"] != "parent_owned" {
			t.Fatalf("missing parent ownership: %s", rr.Body.String())
		}
	}
}

func TestOrdinarySessionStillAcceptsInputIncludingAgentLikeIDs(t *testing.T) {
	_, store := testServer(t)
	provider := &incompleteTurnProvider{activationTestProvider: activationTestProvider{name: "test", model: "model"}, stopReason: "end_turn"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	s := NewServer("127.0.0.1:0", loop, store)
	id := "agt-root"
	if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "test", Model: "model", System: "system", WorkDir: t.TempDir(), Mode: string(permission.ModeAsk)}); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct{ path, body string }{
		{"/api/sessions/activate", `{"id":"agt-root"}`},
		{"/api/turns", `{"sessionId":"agt-root","input":"continue"}`},
	} {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, request.path, strings.NewReader(request.body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("normal root %s returned %d: %s", request.path, rr.Code, rr.Body.String())
		}
	}
}

func TestParentOwnedSubAgentCapabilityIsRetainedInSSE(t *testing.T) {
	s, store := testServer(t)
	newWebConversationTranscript(t, store.Dir, "child", "parent", nil)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/subagents/child/events?sessionId=parent", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("events=%d %s", rr.Code, rr.Body.String())
	}
	for _, field := range []string{`"parentSessionId":"parent"`, `"ownership":"parent"`, `"canAcceptDirectInput":false`, `"directInputBlockedReason":"parent_owned"`} {
		if !strings.Contains(rr.Body.String(), field) {
			t.Fatalf("SSE missing %s: %s", field, rr.Body.String())
		}
	}
	view, ok := s.subAgentDetailView("parent", "child")
	if !ok {
		t.Fatal("child detail unavailable")
	}
	s.enrichSubAgentDetail(&view, false)
	delta := subAgentDeltaPayload(view, "continuing")
	if delta["canAcceptDirectInput"] != false || delta["ownership"] != "parent" || delta["parentSessionId"] != "parent" {
		t.Fatalf("delta lost ownership: %#v", delta)
	}
}
