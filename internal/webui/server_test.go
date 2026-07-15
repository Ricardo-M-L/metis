package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func testServer(t *testing.T) (*Server, *session.Store) {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewServer("127.0.0.1:0", nil, store), store
}

func TestHealthAndSecurityHeaders(t *testing.T) {
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := rr.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q", got)
	}
}

func TestSessionCreateListAndLoad(t *testing.T) {
	s, store := testServer(t)
	h := s.handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewBufferString(`{"model":"test-model"}`)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rr.Code, rr.Body.String())
	}
	var created sessionItem
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Model != "test-model" {
		t.Fatalf("created = %+v", created)
	}
	if err := store.AppendMessage(created.ID, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}); err != nil {
		t.Fatal(err)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(created.ID)) {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions/"+created.ID, nil))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte("hello")) {
		t.Fatalf("load: %d %s", rr.Code, rr.Body.String())
	}
}

func TestSessionAPIRejectsBadMethodsAndIDs(t *testing.T) {
	s, _ := testServer(t)
	h := s.handler()
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodDelete, "/api/sessions", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/sessions/id", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/sessions/a/b", http.StatusBadRequest},
		{http.MethodGet, "/api/sessions/missing", http.StatusNotFound},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rr.Code, tc.want)
		}
	}
}

func TestTurnRequiresRuntimeAndInput(t *testing.T) {
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(`{"input":"hello"}`)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestUnsafeAPIsRejectCrossOriginBrowserRequests(t *testing.T) {
	s, _ := testServer(t)
	h := s.handler()
	for _, tc := range []struct {
		name   string
		header string
		value  string
	}{
		{name: "origin", header: "Origin", value: "https://attacker.example"},
		{name: "fetch metadata", header: "Sec-Fetch-Site", value: "cross-site"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/sessions", bytes.NewBufferString(`{"model":"test"}`))
			req.Header.Set(tc.header, tc.value)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestUnsafeAPIsAllowSameOriginAndNonBrowserClients(t *testing.T) {
	s, _ := testServer(t)
	h := s.handler()
	for _, origin := range []string{"", "http://127.0.0.1:8080"} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/sessions", bytes.NewBufferString(`{"model":"test"}`))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("origin %q status = %d, want 201: %s", origin, rr.Code, rr.Body.String())
		}
	}
}
