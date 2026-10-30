package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func sessionTimeAPIServer(t *testing.T) (*Server, *session.Store) {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2024, 3, 2, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"time-a", "time-b"} {
		if err := store.WriteHeaderFull(session.Header{ID: id, CreatedAt: created.Add(time.Duration(i) * time.Hour),
			Provider: "time-wire", Model: "time-model", System: "system", WorkDir: t.TempDir(),
			Mode: string(permission.ModeDefault), Status: "idle"}); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage(id, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: id}}}); err != nil {
			t.Fatal(err)
		}
		updated := created.Add(24*time.Hour + time.Duration(i)*time.Minute)
		if err := os.Chtimes(filepath.Join(store.Dir, id+".jsonl"), updated, updated); err != nil {
			t.Fatal(err)
		}
	}
	provider := &activationTestProvider{name: "time-wire", model: "time-model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "system", 2)
	loop.Model = "time-model"
	_, history, err := store.Load("time-a")
	if err != nil {
		t.Fatal(err)
	}
	loop.Restore(history)
	return NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		InitialSessionID: "time-a", ProviderName: "time-wire", FreshPermissionMode: permission.ModeDefault,
	}), store
}

func sessionTimeAPIRequest(t *testing.T, s *Server, method, path, body string) sessionItem {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("0001-01-01")) {
		t.Fatalf("%s %s exposed a Go zero date: %s", method, path, rr.Body.String())
	}
	var payload struct {
		Session sessionItem `json:"session"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Session
}

func sessionTimeAPIListEntry(t *testing.T, store *session.Store, id string) session.ListEntry {
	t.Helper()
	entries, err := store.ListResumable(session.ResumeListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.ID == id {
			return entry
		}
	}
	t.Fatalf("session %s absent from persisted list", id)
	return session.ListEntry{}
}

func TestSessionTimeAPIDetailsAndActivationUsePersistedTimestamp(t *testing.T) {
	for _, endpoint := range []struct{ name, method, path, body string }{
		{"details", http.MethodGet, "/api/sessions/time-a", ""},
		{"activate", http.MethodPost, "/api/sessions/activate", `{"id":"time-a"}`},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			s, store := sessionTimeAPIServer(t)
			before := sessionTimeAPIListEntry(t, store, "time-a")
			for range 2 {
				got := sessionTimeAPIRequest(t, s, endpoint.method, endpoint.path, endpoint.body)
				if !got.CreatedAt.Equal(before.CreatedAt) || !got.UpdatedAt.Equal(before.UpdatedAt) {
					t.Fatalf("navigation changed persisted timestamps: response=%+v want created=%s updated=%s", got, before.CreatedAt, before.UpdatedAt)
				}
				after := sessionTimeAPIListEntry(t, store, "time-a")
				if !after.UpdatedAt.Equal(before.UpdatedAt) {
					t.Fatalf("read-only navigation changed activity time: before=%s after=%s", before.UpdatedAt, after.UpdatedAt)
				}
			}
		})
	}
}

func TestSessionTimeAPISwitchingSessionsMatchesEachPersistedFile(t *testing.T) {
	s, store := sessionTimeAPIServer(t)
	for _, id := range []string{"time-a", "time-b", "time-a", "time-b"} {
		before := sessionTimeAPIListEntry(t, store, id)
		got := sessionTimeAPIRequest(t, s, http.MethodPost, "/api/sessions/activate", `{"id":"`+id+`"}`)
		listed := sessionTimeAPIListEntry(t, store, id)
		if got.ID != id || !got.CreatedAt.Equal(listed.CreatedAt) || !got.UpdatedAt.Equal(listed.UpdatedAt) || !got.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("session %s response not tied to its persisted file: response=%+v before=%+v list=%+v", id, got, before, listed)
		}
	}
}

func TestSessionTimeAPIMissingFileFallsBackToKnownCreationTime(t *testing.T) {
	s, _ := sessionTimeAPIServer(t)
	created := time.Date(2024, 3, 2, 10, 0, 0, 0, time.UTC)
	if got := s.sessionUpdatedAt("missing", created); !got.Equal(created) {
		t.Fatalf("missing file fabricated activity time: got=%s want=%s", got, created)
	}
	if got := (*Server)(nil).sessionUpdatedAt("missing", created); !got.Equal(created) {
		t.Fatalf("unavailable store fabricated activity time: got=%s want=%s", got, created)
	}
}
