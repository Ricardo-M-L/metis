package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/computeruse"
)

func TestComputerUseHTTPUsesExplicitRuntimeActions(t *testing.T) {
	var actions []string
	want := computeruse.Status{Installed: true, Version: "1.2.3", Source: "local", Message: "Permissions not granted"}
	s := NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{
		ComputerUse: func(ctx context.Context, action string) (computeruse.Status, error) {
			if ctx == nil {
				t.Fatal("missing request context")
			}
			actions = append(actions, action)
			return want, nil
		},
	})
	handler := s.handler()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/computer-use", nil))
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status response: %d %s", rr.Code, rr.Body.String())
	}
	var got computeruse.Status
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("status = %+v, err = %v", got, err)
	}
	if !reflect.DeepEqual(actions, []string{"status"}) {
		t.Fatalf("GET performed a mutation: %v", actions)
	}
	for _, action := range []string{"install", "enable", "stop", "disable", "permissions-accessibility", "permissions-screen-recording"} {
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/computer-use", strings.NewReader(`{"action":"`+action+`"}`)))
		if rr.Code != http.StatusOK || actions[len(actions)-1] != action {
			t.Fatalf("action %s: %d %s; callbacks %v", action, rr.Code, rr.Body.String(), actions)
		}
	}
}

func TestComputerUseHTTPRejectsUntrustedActionsBeforeCallback(t *testing.T) {
	calls := 0
	s := &Server{computerUse: func(context.Context, string) (computeruse.Status, error) {
		calls++
		return computeruse.Status{}, nil
	}}
	for _, body := range []string{
		``, `{}`, `null`, `[]`, `{"action":null}`, `{"action":5}`,
		`{"action":""}`, `{"action":"status"}`, `{"action":"/tmp/helper"}`,
		`{"action":"enable","path":"/tmp/helper"}`, `{"path":"/tmp/helper","action":"enable"}`,
		`{"action":"enable","action":"stop"}`, `{"action":"enable"} {}`, `{"action":"enable"} garbage`,
	} {
		t.Run(body, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.handleComputerUse(rr, httptest.NewRequest(http.MethodPost, "/api/computer-use", strings.NewReader(body)))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
		})
	}
	rr := httptest.NewRecorder()
	s.handleComputerUse(rr, httptest.NewRequest(http.MethodPost, "/api/computer-use", strings.NewReader(`{"action":"enable"}`+strings.Repeat(" ", computerUseRequestLimit))))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d", rr.Code)
	}
	if calls != 0 {
		t.Fatalf("rejected input invoked callback %d times", calls)
	}
}

func TestComputerUseHTTPAvailabilityAndOriginGuards(t *testing.T) {
	s := &Server{}
	for method, want := range map[string]int{http.MethodGet: 503, http.MethodPost: 503, http.MethodPut: 405, http.MethodDelete: 405} {
		rr := httptest.NewRecorder()
		s.handleComputerUse(rr, httptest.NewRequest(method, "/api/computer-use", nil))
		if rr.Code != want {
			t.Fatalf("%s status = %d, want %d", method, rr.Code, want)
		}
	}
	calls := 0
	s.computerUse = func(context.Context, string) (computeruse.Status, error) {
		calls++
		return computeruse.Status{}, errors.New("helper failed its compatibility check")
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		r := httptest.NewRequest(method, "http://127.0.0.1:7777/api/computer-use", strings.NewReader(`{"action":"enable"}`))
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, r)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("cross-site %s status = %d", method, rr.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7777/api/computer-use", strings.NewReader(`{"action":"enable"}`))
	r.Header.Set("Origin", "https://unrelated.example")
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusForbidden || calls != 0 {
		t.Fatalf("origin guard = %d; callback calls = %d", rr.Code, calls)
	}
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/computer-use", nil))
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "compatibility check") {
		t.Fatalf("callback error = %d %s", rr.Code, rr.Body.String())
	}
}
