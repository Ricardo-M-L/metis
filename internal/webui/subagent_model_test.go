package webui

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
)

func TestSubAgentDetailRejectsRosterReplacementAfterOwnerValidation(t *testing.T) {
	for _, nextParent := range []string{"parent-b", "parent-a"} {
		t.Run(nextParent, func(t *testing.T) {
			_, store := testServer(t)
			id := "agt-owner-generation"
			original := newWebConversationTranscript(t, store.Dir, id, "parent-a", nil)
			if err := original.Close(); err != nil {
				t.Fatal(err)
			}
			roster := agent.NewRoster(2)
			first := &agent.Teammate{Name: "old-run", AgentID: id, Started: time.Now()}
			first.AppendText("old parent-a output")
			if err := roster.Register(first); err != nil {
				t.Fatal(err)
			}
			s := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{Roster: roster})
			view, found := s.readSubAgentDetailView("parent-a", id, func() {
				roster.UnregisterTeammate(first)
				replacement := &agent.Teammate{Name: "replacement-run", AgentID: id, Started: time.Now()}
				replacement.AppendText("replacement private output")
				if err := roster.Register(replacement); err != nil {
					t.Fatal(err)
				}
				newWebConversationTranscript(t, store.Dir, id, nextParent, nil)
			})
			if found {
				t.Fatalf("request validated the old generation but returned replacement roster output: session=%q output=%q", view.SessionID, view.Output)
			}
		})
	}
}

func TestSubAgentModelComesFromValidatedChildHeader(t *testing.T) {
	for _, source := range []string{"worker", "roster", "disk", "unknown-worker"} {
		t.Run(source, func(t *testing.T) {
			_, store := testServer(t)
			const id, parent, childModel = "agt-model", "parent", "child-model-actual"
			var transcript *agent.SubAgentTranscript
			if source != "unknown-worker" {
				var err error
				transcript, err = agent.NewSubAgentTranscript(store.Dir, id, agent.NewSubAgentHeader(id, childModel, parent, "child", "", "default"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = transcript.Close() })
			}
			roster := agent.NewRoster(2)
			var teammate *agent.Teammate
			if source == "roster" {
				teammate = &agent.Teammate{Name: "child", AgentID: id, Started: time.Now()}
				if err := roster.Register(teammate); err != nil {
					t.Fatal(err)
				}
			}
			s := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{Roster: roster})
			s.activeModel = "different-parent-model-must-not-be-inferred"
			if source == "worker" || source == "unknown-worker" {
				s.setWorkerSnapshot(parent, desktopipc.Status{Agents: []desktopipc.Subagent{{AgentID: id, Status: "running"}}})
			}
			wantModel := childModel
			if source == "unknown-worker" {
				wantModel = ""
			}
			for _, path := range []string{"/api/subagents/" + id + "?sessionId=" + parent, "/api/subagents/" + id + "/conversation?sessionId=" + parent} {
				rr := httptest.NewRecorder()
				s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
				var payload struct {
					Agent map[string]any `json:"agent"`
				}
				if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &payload) != nil || payload.Agent["model"] != wantModel {
					t.Fatalf("model must come from this child header (%s): status=%d payload=%s", source, rr.Code, rr.Body.String())
				}
			}
			if source == "worker" || source == "unknown-worker" {
				s.setWorkerSnapshot(parent, desktopipc.Status{Agents: []desktopipc.Subagent{{AgentID: id, Status: "completed"}}})
			} else if teammate != nil {
				teammate.Finish(agent.StatusCompleted, "done", nil, "end_turn")
			} else if err := transcript.AppendTerminal(agent.SubAgentTerminal{Status: "completed", EndedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			rr := httptest.NewRecorder()
			s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/subagents/"+id+"/events?sessionId="+parent, nil))
			event, payload := nextSubAgentSSE(t, bufio.NewReader(rr.Body))
			if event != "snapshot" || payload["agent"].(map[string]any)["model"] != wantModel {
				t.Fatalf("SSE snapshot model=%v event=%s want=%s", payload, event, wantModel)
			}
			view, ok := s.subAgentDetailView(parent, id)
			if !ok || subAgentDeltaPayload(view, "delta")["model"] != wantModel {
				t.Fatalf("SSE delta model did not retain the validated child model: %+v", view)
			}
		})
	}
}

func TestSubAgentConversationRevisionMatchesStaticDetailAndSSE(t *testing.T) {
	s, store := testServer(t)
	const id, parent = "agt-revision-parity", "parent"
	transcript := newWebConversationTranscript(t, store.Dir, id, parent, nil)
	if err := transcript.AppendTerminal(agent.SubAgentTerminal{Status: "completed", EndedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	assertParity := func(stage string) subAgentConversationView {
		t.Helper()
		code, conversation, raw := conversationHTTP(t, s, "/api/subagents/"+id+"/conversation?sessionId="+parent)
		if code != http.StatusOK || conversation.Revision == "" {
			t.Fatalf("%s conversation=%d %s", stage, code, raw)
		}
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/subagents/"+id+"?sessionId="+parent, nil))
		var detail struct {
			Agent subAgentDetailView `json:"agent"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &detail) != nil || detail.Agent.ConversationRevision != conversation.Revision {
			t.Fatalf("%s static detail revision differs: conversation=%q detail=%s", stage, conversation.Revision, rr.Body.String())
		}
		rr = httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/subagents/"+id+"/events?sessionId="+parent, nil))
		event, payload := nextSubAgentSSE(t, bufio.NewReader(rr.Body))
		if event != "snapshot" || payload["agent"].(map[string]any)["conversationRevision"] != conversation.Revision {
			t.Fatalf("%s static SSE revision differs: conversation=%q SSE=%v", stage, conversation.Revision, payload)
		}
		if subAgentDeltaPayload(detail.Agent, "text")["conversationRevision"] != conversation.Revision {
			t.Fatalf("%s static delta revision differs", stage)
		}
		return conversation
	}
	initial := assertParity("no usage or activity")
	if initial.Usage.Available || initial.Activity.Available {
		t.Fatal("fixture must begin without receipts or activity")
	}
	activity, err := agent.NewSubAgentActivityRecorder(store.Dir, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = activity.Close() })
	if body := assertParity("empty activity run"); !body.Activity.Available || body.Revision == initial.Revision {
		t.Fatal("activity-only generation must invalidate the revision")
	}
	header, err := agent.LoadSubAgentHeader(store.Dir, id)
	if err != nil {
		t.Fatal(err)
	}
	encodedHeader, _ := json.Marshal(header)
	digest := sha256.Sum256(encodedHeader)
	usage := map[string]any{"version": 1, "agent_id": id, "parent_session_id": parent, "header_sha256": fmt.Sprintf("%x", digest), "receipt_id": "parity-receipt", "reported": true, "input_tokens": 2, "output_tokens": 1, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 4}
	encodedUsage, _ := json.Marshal(usage)
	usagePath := filepath.Join(store.Dir, "subagent-usage", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(usagePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(usagePath, append(encodedUsage, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if body := assertParity("usage and activity"); !body.Usage.Available || body.Usage.TotalTokens == nil || *body.Usage.TotalTokens != 10 {
		t.Fatalf("fixture usage not consumed: %+v", body.Usage)
	}
	for _, event := range []agent.Event{
		{Kind: agent.EventToolStart, TraceCallID: "revision-call", ToolUseID: "provider-call", ToolName: "Read", ToolInput: map[string]any{"path": "fixture"}},
		{Kind: agent.EventToolResult, TraceCallID: "revision-call", ToolUseID: "provider-call", ToolName: "Read", ToolResult: &agent.ToolResult{Output: "complete output"}},
	} {
		if err := activity.Observe(event); err != nil {
			t.Fatal(err)
		}
		assertParity(fmt.Sprint(event.Kind))
	}
	if err := activity.Close(); err != nil {
		t.Fatal(err)
	}
	assertParity("closed activity")
}
