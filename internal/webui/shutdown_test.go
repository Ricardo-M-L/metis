package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

const testDesktopShutdownToken = "native-desktop-secret"

func TestDesktopShutdownEndpointIsUnavailableWithoutNativeToken(t *testing.T) {
	server := NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{
		Shutdown: func() { t.Error("browser-mode shutdown callback must not run") },
	})

	request := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	request.Host = "127.0.0.1:8080"
	response := httptest.NewRecorder()
	server.handler().ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d when native shutdown is disabled", response.Code, http.StatusNotFound)
	}
}

func TestDesktopShutdownEndpointRequiresNativeToken(t *testing.T) {
	called := make(chan struct{}, 1)
	server := NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{
		ShutdownToken: testDesktopShutdownToken,
		Shutdown:      func() { called <- struct{}{} },
	})

	for _, token := range []string{"", "wrong-token"} {
		request := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
		request.Host = "127.0.0.1:8080"
		request.Header.Set(desktopShutdownTokenHeader, token)
		response := httptest.NewRecorder()
		server.handler().ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("token %q status = %d, want %d", token, response.Code, http.StatusForbidden)
		}
	}

	select {
	case <-called:
		t.Fatal("shutdown callback ran for a missing or incorrect token")
	default:
	}
}

func TestDesktopShutdownEndpointCancelsServerWithNativeToken(t *testing.T) {
	called := make(chan struct{}, 1)
	server := NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{
		ShutdownToken: testDesktopShutdownToken,
		Shutdown:      func() { called <- struct{}{} },
	})
	request := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	request.Host = "127.0.0.1:8080"
	request.Header.Set(desktopShutdownTokenHeader, testDesktopShutdownToken)
	response := httptest.NewRecorder()

	server.handler().ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusAccepted)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("authorized shutdown did not cancel the server")
	}
}

func TestDesktopShutdownEndpointRejectsNonLoopbackHost(t *testing.T) {
	called := make(chan struct{}, 1)
	server := NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{
		ShutdownToken: testDesktopShutdownToken,
		Shutdown:      func() { called <- struct{}{} },
	})
	request := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	request.Host = "example.com"
	request.Header.Set(desktopShutdownTokenHeader, testDesktopShutdownToken)
	response := httptest.NewRecorder()

	server.handler().ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d for a non-loopback Host", response.Code, http.StatusForbidden)
	}
	select {
	case <-called:
		t.Fatal("shutdown callback ran for a non-loopback request")
	default:
	}
}

func TestDesktopShutdownCancelsActiveTurnBeforeServer(t *testing.T) {
	turnCtx, cancelTurn := context.WithCancel(context.Background())
	defer cancelTurn()
	turnDone := make(chan struct{})
	go func() {
		<-turnCtx.Done()
		close(turnDone)
	}()
	permissionReply := make(chan agent.PermissionDecision, 1)
	askReply := make(chan string, 1)
	shutdownCalled := make(chan struct{}, 1)
	server := NewServer("127.0.0.1:0", nil, nil, RuntimeBindings{
		ShutdownToken: testDesktopShutdownToken,
		Shutdown: func() {
			select {
			case <-turnDone:
				shutdownCalled <- struct{}{}
			default:
				t.Error("server shutdown ran before the active turn stopped")
			}
		},
	})
	server.cancelTurn = cancelTurn
	server.turnDone = turnDone
	server.pendingPerms["permission"] = &permissionPending{reply: permissionReply}
	server.pendingAsks["ask"] = &askPending{reply: askReply}

	request := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	request.Host = "127.0.0.1:8080"
	request.Header.Set(desktopShutdownTokenHeader, testDesktopShutdownToken)
	response := httptest.NewRecorder()
	server.handler().ServeHTTP(response, request)

	select {
	case <-shutdownCalled:
	case <-time.After(time.Second):
		t.Fatal("server shutdown did not wait for active turn cancellation")
	}
	select {
	case decision := <-permissionReply:
		if decision != agent.PermissionDecisionDeny {
			t.Fatalf("permission reply = %v, want deny", decision)
		}
	default:
		t.Fatal("pending permission was not released")
	}
	select {
	case answer := <-askReply:
		if answer != "" {
			t.Fatalf("ask reply = %q, want empty cancellation", answer)
		}
	default:
		t.Fatal("pending AskUser interaction was not released")
	}
}
