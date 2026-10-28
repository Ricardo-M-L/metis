package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

type webSuggestionProvider struct {
	activationTestProvider
	calls    int
	complete func(context.Context, llm.Request) (*llm.Response, error)
}

func (p *webSuggestionProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	p.calls++
	if p.complete != nil {
		return p.complete(ctx, req)
	}
	return &llm.Response{StopReason: "end_turn", Content: []llm.ContentBlock{{Type: "text", Text: "补充回归测试"}}}, nil
}

func newWebSuggestionFixture(t *testing.T) (*Server, *session.Store, string, *webSuggestionProvider) {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	s, store := testServer(t)
	id := store.NewSessionID()
	if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "session-provider", Model: "session-model", WorkDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "修复输入框"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "输入框已修复"}}},
	} {
		if err := store.AppendMessage(id, msg); err != nil {
			t.Fatal(err)
		}
	}
	p := &webSuggestionProvider{activationTestProvider: activationTestProvider{name: "session-provider", model: "session-model"}}
	s.activeSessionID = "some-other-active-session"
	s.buildProvider = func(name, model string) (*rtpkg.ProviderBuild, error) {
		if name != "session-provider" || model != "session-model" {
			t.Errorf("suggestion used wrong session route %q/%q", name, model)
		}
		return &rtpkg.ProviderBuild{Provider: p, Model: model}, nil
	}
	return s, store, id, p
}

func requestWebSuggestion(t *testing.T, s *Server, id string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"sessionId": id})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/prompt-suggestion", strings.NewReader(string(body))))
	if rr.Code != 200 {
		t.Fatalf("suggestion status=%d: %s", rr.Code, rr.Body.String())
	}
	var response struct {
		Suggestion string `json:"suggestion"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Suggestion
}

func TestPromptSuggestionUsesRequestedSessionWithoutMutatingHistory(t *testing.T) {
	s, store, id, p := newWebSuggestionFixture(t)
	beforeHeader, beforeHistory, _ := store.Load(id)
	p.complete = func(_ context.Context, req llm.Request) (*llm.Response, error) {
		if req.Model != "session-model" || len(req.Tools) != 0 || !strings.Contains(req.Messages[0].Content[0].Text, "输入框已修复") {
			t.Fatalf("incorrect session suggestion request: %#v", req)
		}
		return &llm.Response{StopReason: "end_turn", Content: []llm.ContentBlock{{Type: "text", Text: "补充回归测试"}}}, nil
	}
	if text := requestWebSuggestion(t, s, id); text != "补充回归测试" {
		t.Fatalf("suggestion=%q", text)
	}
	afterHeader, afterHistory, _ := store.Load(id)
	if !reflect.DeepEqual(beforeHeader, afterHeader) || !reflect.DeepEqual(beforeHistory, afterHistory) || s.activeSessionID != "some-other-active-session" {
		t.Fatal("suggestion changed the session or activated another runtime")
	}
}

func TestPromptSuggestionRejectsBusyDisabledAndIncompleteSessions(t *testing.T) {
	for _, state := range []string{"busy", "disabled", "incomplete", "unavailable route"} {
		t.Run(state, func(t *testing.T) {
			s, store, id, p := newWebSuggestionFixture(t)
			switch state {
			case "busy":
				lock, ok, err := store.TryAcquireTurnLock(id)
				if err != nil || !ok {
					t.Fatalf("turn lock: %v, %v", ok, err)
				}
				defer lock.Release()
			case "disabled":
				if err := config.SaveUserSettings([]config.UserSetting{{Key: "ui.prompt_suggestions", Value: "false"}}); err != nil {
					t.Fatal(err)
				}
			case "incomplete":
				if err := store.AppendMessage(id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "new task"}}}); err != nil {
					t.Fatal(err)
				}
			case "unavailable route":
				s.buildProvider = nil
			}
			if text := requestWebSuggestion(t, s, id); text != "" || p.calls != 0 {
				t.Fatalf("ineligible state called provider or exposed text: calls=%d text=%q", p.calls, text)
			}
		})
	}
}

func TestPromptSuggestionDiscardsChangedHistoryAndProviderErrors(t *testing.T) {
	for _, state := range []string{"changed history", "provider error", "disabled during generation"} {
		t.Run(state, func(t *testing.T) {
			s, store, id, p := newWebSuggestionFixture(t)
			p.complete = func(context.Context, llm.Request) (*llm.Response, error) {
				switch state {
				case "changed history":
					if err := store.AppendMessage(id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "next task"}}}); err != nil {
						t.Fatal(err)
					}
				case "provider error":
					return nil, errors.New("provider failed")
				case "disabled during generation":
					if err := config.SaveUserSettings([]config.UserSetting{{Key: "ui.prompt_suggestions", Value: "false"}}); err != nil {
						t.Fatal(err)
					}
				}
				return &llm.Response{StopReason: "end_turn", Content: []llm.ContentBlock{{Type: "text", Text: "stale suggestion"}}}, nil
			}
			if got := requestWebSuggestion(t, s, id); got != "" {
				t.Fatalf("stale or failed generation surfaced: %q", got)
			}
		})
	}
}

func TestPromptSuggestionValidatesRequestAndSetting(t *testing.T) {
	s, _, id, _ := newWebSuggestionFixture(t)
	for _, tc := range []struct {
		method, body string
		status       int
	}{
		{"GET", "", 405}, {"POST", "{}", 400}, {"POST", `{"sessionId":"../outside"}`, 400},
	} {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(tc.method, "/api/prompt-suggestion", strings.NewReader(tc.body)))
		if rr.Code != tc.status {
			t.Errorf("%s %s status=%d, want %d", tc.method, tc.body, rr.Code, tc.status)
		}
	}
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"changes":[{"key":"ui.prompt_suggestions","value":"false"}]}`)))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"liveApplied":["ui.prompt_suggestions"]`) {
		t.Fatalf("setting not applied live: %d %s", rr.Code, rr.Body.String())
	}
	if got := requestWebSuggestion(t, s, id); got != "" {
		t.Fatalf("disabled endpoint returned %q", got)
	}
}
