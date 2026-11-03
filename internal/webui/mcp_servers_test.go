package webui

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPServersEndpointReadOnlyAndRedactsErrors(t *testing.T) {
	secret := "sensitive-credential-in-internal-error"
	called := 0
	s := NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{MCPServersSnapshot: func() (MCPServersSnapshot, error) {
		called++
		return MCPServersSnapshot{}, errors.New(secret)
	}})
	handler := s.handler()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/mcp/servers", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), secret) || called != 1 {
		t.Fatalf("status=%d body=%q calls=%d", response.Code, response.Body.String(), called)
	}
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/mcp/servers", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || called != 1 {
		t.Fatalf("POST status=%d calls=%d", response.Code, called)
	}
	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/mcp/servers", nil)
	request.Header.Set("Origin", "https://evil.example")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || called != 1 {
		t.Fatalf("cross-origin status=%d calls=%d", response.Code, called)
	}
}

func TestMCPServersEndpointReturnsScopedSnapshot(t *testing.T) {
	s := NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{MCPServersSnapshot: func() (MCPServersSnapshot, error) {
		return MCPServersSnapshot{Servers: []MCPServerView{{Name: "sample", Source: "user", Transport: "stdio", Enabled: true, Status: "lazy", ToolCount: 2}}, PluginScanIncomplete: true}, nil
	}})
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/mcp/servers", nil)
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, request)
	body, _ := io.ReadAll(response.Result().Body)
	if response.Code != http.StatusOK || !strings.Contains(string(body), `"statusScope":"desktop-process"`) || !strings.Contains(string(body), `"status":"lazy"`) || !strings.Contains(string(body), `"toolCount":2`) || !strings.Contains(string(body), `"pluginScanIncomplete":true`) {
		t.Fatalf("status=%d body=%s", response.Code, body)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("snapshot should not be cached")
	}
}
