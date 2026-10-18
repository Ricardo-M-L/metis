package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

func isolateDesktopFirstRun(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	t.Setenv("METIS_AUTO_MEMORY", "0")
	t.Setenv("METIS_SIMPLE", "")
	t.Setenv("METIS_COORDINATOR_MODE", "")
	for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		t.Setenv(name, "")
	}
	t.Chdir(t.TempDir())
}

func TestDesktopFirstRunSettingsOpenAndProviderCanBeConfigured(t *testing.T) {
	isolateDesktopFirstRun(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cmdDesktop(ctx, []string{"--web", "--port", port}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Desktop shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Desktop did not stop after cancellation")
		}
	})
	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://127.0.0.1:" + port
	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case err := <-done:
			// Keep cleanup's join deterministic after an early startup failure.
			done <- nil
			t.Fatalf("credential-free Desktop must open settings: %v", err)
		default:
		}
		response, err := client.Get(base + "/api/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Desktop settings did not become ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	request := func(method, path, body string) []byte {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			t.Fatalf("%s %s: HTTP %d %s", method, path, resp.StatusCode, data)
		}
		return data
	}
	for _, path := range []string{"/", "/api/config", "/api/providers"} {
		request(http.MethodGet, path, "")
	}
	var providerRequests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		providerRequests.Add(1)
	}))
	defer provider.Close()
	body, err := json.Marshal(map[string]string{
		"id": "desktop-first-run", "transport": "openai_chat",
		"baseUrl": provider.URL + "/v1", "model": "first-run-model", "apiKey": "test-first-run-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	request(http.MethodPost, "/api/providers", string(body))
	request(http.MethodPost, "/api/models", `{"provider":"desktop-first-run","model":"first-run-model"}`)
	var selected struct {
		Current struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"current"`
	}
	if err := json.Unmarshal(request(http.MethodGet, "/api/models", ""), &selected); err != nil {
		t.Fatal(err)
	}
	if selected.Current.Provider != "desktop-first-run" || selected.Current.Model != "first-run-model" {
		t.Fatalf("settings did not replace the auth-required provider: %+v", selected.Current)
	}
	if providerRequests.Load() != 0 {
		t.Fatal("saving or locally selecting credentials unexpectedly contacted the model endpoint")
	}
	if _, err := os.Stat(auth.Path()); err != nil {
		t.Fatalf("provider credentials were not persisted by the existing settings API: %v", err)
	}
}

func TestDesktopFirstRunMissingAuthCannotSendModelRequests(t *testing.T) {
	isolateDesktopFirstRun(t)
	oldTTY, oldWizard := authGateIsTTY, authGateRunWizard
	t.Cleanup(func() { authGateIsTTY, authGateRunWizard = oldTTY, oldWizard })
	authGateIsTTY = func() bool { return true }
	authGateRunWizard = func() (*rtpkg.WizardResult, error) {
		t.Error("Desktop startup must never run a terminal credential wizard")
		return nil, errors.New("unexpected wizard")
	}
	rt, err := setupRuntime(context.Background(), &cliFlags{bare: true, desktopSettingsStartup: true})
	if err != nil {
		t.Fatalf("Desktop missing-auth startup: %v", err)
	}
	defer rt.Cleanup()
	if rt.providerName != "anthropic" || rt.provider.Name() != "anthropic" || rt.model != rt.cfg.Provider.Anthropic.Model {
		t.Fatalf("missing-auth startup changed provider/model: %q %q %q", rt.providerName, rt.provider.Name(), rt.model)
	}
	if rt.gate.Mode() != permission.ModeDefault || rt.sandbox.State().FullAccessRequired {
		t.Fatalf("missing-auth startup weakened permissions: %q %+v", rt.gate.Mode(), rt.sandbox.State())
	}
	for _, stream := range []bool{false, true} {
		var requestErr error
		if stream {
			_, requestErr = rt.provider.Stream(context.Background(), llm.Request{})
		} else {
			_, requestErr = rt.provider.Complete(context.Background(), llm.Request{})
		}
		if !errors.Is(requestErr, config.ErrMissingAPIKey) || !strings.Contains(requestErr.Error(), "Desktop Settings") {
			t.Fatalf("model request error = %v, want actionable missing auth", requestErr)
		}
	}
	if _, err := setupRuntime(context.Background(), &cliFlags{bare: true, noAuthWizard: true}); !errors.Is(err, config.ErrMissingAPIKey) {
		t.Fatalf("ordinary CLI missing-auth startup = %v, want unchanged auth gate", err)
	}
}

func TestDesktopFirstRunDoesNotHideInvalidConfigurationOrCredentials(t *testing.T) {
	for _, test := range []struct {
		name     string
		config   string
		auth     string
		oauth    string
		provider string
	}{
		{name: "invalid-provider", provider: "unknown-desktop-provider"},
		{name: "malformed-config", config: "[invalid"},
		{name: "corrupt-credential-store", auth: "{"},
		{name: "corrupt-oauth-store", oauth: "{"},
		{name: "unsafe-endpoint", config: "[provider.anthropic]\nbase_url = \"http://untrusted.example/v1\"\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateDesktopFirstRun(t)
			if err := os.MkdirAll(config.Home(), 0o700); err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string]string{"config.toml": test.config, "auth.json": test.auth, "llm-oauth.json": test.oauth} {
				if value != "" {
					if err := os.WriteFile(filepath.Join(config.Home(), name), []byte(value), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			rt, err := setupRuntime(context.Background(), &cliFlags{bare: true, provider: test.provider, desktopSettingsStartup: true})
			if rt != nil {
				rt.Cleanup()
			}
			if err == nil || errors.Is(err, config.ErrMissingAPIKey) {
				t.Fatalf("invalid Desktop startup returned %v; want non-auth configuration failure", err)
			}
		})
	}
}

func TestDesktopFirstRunStillChecksHostSandbox(t *testing.T) {
	isolateDesktopFirstRun(t)
	oldCheck := startupRequireCredentialIsolation
	t.Cleanup(func() { startupRequireCredentialIsolation = oldCheck })
	want := errors.New("host isolation is unavailable")
	called := false
	startupRequireCredentialIsolation = func(*sandbox.Manager, bool) error {
		called = true
		return want
	}
	rt, err := setupRuntime(context.Background(), &cliFlags{bare: true, desktopSettingsStartup: true})
	if rt != nil {
		rt.Cleanup()
	}
	if !called || !errors.Is(err, want) {
		t.Fatalf("Desktop startup skipped host checks: called=%v, error=%v", called, err)
	}
}
