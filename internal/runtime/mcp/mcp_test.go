package mcp

import (
	"context"
	"encoding/json"
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
	mcpsdk "github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/mcpoauth"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestStdioLaunchEnvAndProfile_ComputerUseCapabilityIsReserved(t *testing.T) {
	t.Setenv("DISPLAY", ":66")

	reservedEnv, reservedProfile := stdioLaunchEnvAndProfile(ServerEntry{
		Name: ReservedComputerUseName, Command: ReservedComputerUseBinary,
	})
	if reservedProfile != mcpsdk.StdioSandboxProfileComputerUse {
		t.Fatalf("reserved profile = %v, want Computer Use", reservedProfile)
	}
	if got := envMapFromSlice(reservedEnv)["DISPLAY"]; got != ":66" {
		t.Fatalf("reserved DISPLAY = %q, want inherited display", got)
	}

	for _, entry := range []ServerEntry{
		{Name: "ordinary", Command: ReservedComputerUseBinary},
		{Name: ReservedComputerUseName, Command: "/tmp/metis-cu"},
	} {
		env, profile := stdioLaunchEnvAndProfile(entry)
		if profile != mcpsdk.StdioSandboxProfileGeneric {
			t.Errorf("masquerade %+v profile = %v, want generic", entry, profile)
		}
		if _, ok := envMapFromSlice(env)["DISPLAY"]; ok {
			t.Errorf("masquerade %+v inherited DISPLAY", entry)
		}
	}
}

func envMapFromSlice(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok {
			out[key] = value
		}
	}
	return out
}

// oauthServerWithoutToken records any attempt to reach the server. A missing
// non-interactive credential must fail before discovery or the MCP endpoint.
func oauthServerWithoutToken(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var mcpRequests atomic.Int32
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_servers": []string{server.URL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 server.URL,
			"authorization_endpoint": server.URL + "/authorize",
			"token_endpoint":         server.URL + "/token",
		})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		mcpRequests.Add(1)
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
	})
	return server.URL + "/mcp", &mcpRequests
}

func requireExplicitMCPLoginError(t *testing.T, message, serverName string) {
	t.Helper()
	for _, want := range []string{
		"OAuth credential is not configured",
		"explicit login is required",
		"/mcp login " + serverName,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("error %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, "metis mcp login") {
		t.Errorf("error %q points to the nonexistent metis mcp login CLI command", message)
	}
}

func TestResolveAuthHeadersPreservesNonOAuthBehavior(t *testing.T) {
	tests := []struct {
		name  string
		entry ServerEntry
	}{
		{
			name:  "unauthenticated HTTP",
			entry: ServerEntry{URL: "https://example.test/mcp"},
		},
		{
			name: "static headers",
			entry: ServerEntry{
				URL:     "https://example.test/mcp",
				Headers: map[string]string{"Authorization": "ApiKey fixed", "X-Tenant": "one"},
			},
		},
		{
			name: "non-OAuth auth label",
			entry: ServerEntry{
				URL:     "https://example.test/mcp",
				Auth:    "static",
				Headers: map[string]string{"X-API-Key": "fixed"},
			},
		},
		{
			name: "stdio ignores OAuth",
			entry: ServerEntry{
				Command: "example-mcp",
				Auth:    "oauth",
				Headers: map[string]string{"X-Unused": "preserved"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveAuthHeaders(context.Background(), tt.entry)
			if err != nil {
				t.Fatalf("resolveAuthHeaders: %v", err)
			}
			if len(got) != len(tt.entry.Headers) {
				t.Fatalf("headers = %#v, want %#v", got, tt.entry.Headers)
			}
			for key, want := range tt.entry.Headers {
				if got[key] != want {
					t.Errorf("header %q = %q, want %q", key, got[key], want)
				}
			}
		})
	}
}

func TestResolveAuthHeadersUsesCachedOAuthToken(t *testing.T) {
	withTempHome(t)
	const serverName = "cached-oauth"
	store := mcpoauth.NewTokenStore()
	serverURL := "http://127.0.0.1:1/not-contacted"
	if err := store.PutEntry(serverName, &mcpoauth.TokenEntry{
		ServerURL: serverURL, ResourceURL: serverURL,
		Issuer: "https://issuer.example/", ClientID: "client",
		AuthURL: "https://issuer.example/authorize", TokenURL: "https://issuer.example/token",
		Token: &auth.Token{AccessToken: "fresh-token", ExpiresAt: time.Now().Add(time.Hour)},
	}); err != nil {
		t.Fatalf("store OAuth token: %v", err)
	}
	static := map[string]string{"Authorization": "Bearer stale", "X-Tenant": "one"}
	got, err := resolveAuthHeaders(context.Background(), ServerEntry{
		Name: serverName, URL: serverURL, Auth: "OAUTH", Headers: static,
	})
	if err != nil {
		t.Fatalf("resolveAuthHeaders: %v", err)
	}
	if got["Authorization"] != "Bearer fresh-token" || got["X-Tenant"] != "one" {
		t.Fatalf("OAuth headers = %#v", got)
	}
	if static["Authorization"] != "Bearer stale" {
		t.Fatalf("OAuth resolution mutated configured headers: %#v", static)
	}
}

func TestResolveAuthHeadersDoesNotMisclassifyStoreCorruptionAsMissing(t *testing.T) {
	home := withTempHome(t)
	if err := os.WriteFile(filepath.Join(home, "mcp-oauth.json"), []byte(`{"broken":`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := resolveAuthHeaders(context.Background(), ServerEntry{
		Name: "broken-store", URL: "https://example.test/mcp", Auth: "oauth",
	})
	if err == nil {
		t.Fatal("corrupt OAuth token store unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "credential is not configured") {
		t.Fatalf("store corruption was misclassified as missing: %v", err)
	}
	if !strings.Contains(err.Error(), "token store") || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("store corruption error is not diagnostic: %v", err)
	}
}

func TestResolveAuthHeadersClassifiesURLBindingMismatchAsRelogin(t *testing.T) {
	withTempHome(t)
	if err := mcpoauth.NewTokenStore().PutEntry("moved", &mcpoauth.TokenEntry{
		ServerURL: "https://old.example.test/mcp", ResourceURL: "https://old.example.test/mcp",
		Issuer: "https://issuer.example/", ClientID: "client",
		AuthURL: "https://issuer.example/authorize", TokenURL: "https://issuer.example/token",
		Token: &auth.Token{AccessToken: "old", ExpiresAt: time.Now().Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := resolveAuthHeaders(context.Background(), ServerEntry{
		Name: "moved", URL: "https://new.example.test/mcp", Auth: "oauth",
	})
	if err == nil {
		t.Fatal("URL binding mismatch unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "credential is not configured") {
		t.Fatalf("URL binding mismatch was misclassified as missing: %v", err)
	}
	for _, want := range []string{"no longer usable", "re-login", "/mcp login moved"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("URL mismatch error %q does not contain %q", err, want)
		}
	}
}

func TestLaunchServerOAuthMissingCredentialFailsBeforeMCPConnect(t *testing.T) {
	withTempHome(t)
	endpoint, mcpRequests := oauthServerWithoutToken(t)
	entry := ServerEntry{Name: "secure-eager", URL: endpoint, Auth: "oauth"}
	reg := &Registry{Servers: []ServerEntry{entry}}

	server, err := LaunchServer(context.Background(), reg, entry.Name, tools.NewRegistry())
	if server != nil {
		_ = server.Close()
		t.Fatal("OAuth launch without a credential unexpectedly returned a server")
	}
	if err == nil {
		t.Fatal("OAuth launch without a credential unexpectedly succeeded")
	}
	requireExplicitMCPLoginError(t, err.Error(), entry.Name)
	if got := mcpRequests.Load(); got != 0 {
		t.Fatalf("OAuth credential failure reached the protected MCP endpoint %d time(s)", got)
	}
}

func TestLazyOAuthMissingCredentialFailsBeforeMCPConnect(t *testing.T) {
	withTempHome(t)
	endpoint, mcpRequests := oauthServerWithoutToken(t)
	entry := ServerEntry{Name: "secure-lazy", URL: endpoint, Auth: "oauth"}
	server := buildLazyServer(entry, entry, []CachedTool{{
		Name:        "probe",
		Description: "exercise lazy OAuth spawn",
		InputSchema: map[string]any{"type": "object"},
	}})
	defer server.Close()

	serverTools := server.Tools()
	if len(serverTools) != 1 {
		t.Fatalf("lazy server tools = %d, want 1", len(serverTools))
	}
	result, err := serverTools[0].Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("lazy tool execution returned transport error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("lazy OAuth launch result = %#v, want tool error", result)
	}
	requireExplicitMCPLoginError(t, result.Output, entry.Name)
	if got := mcpRequests.Load(); got != 0 {
		t.Fatalf("lazy OAuth credential failure reached the protected MCP endpoint %d time(s)", got)
	}
}

func TestLaunchServerWithSandboxFailsClosedBeforeStdioStart(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	entry := ServerEntry{Name: "closed-sandbox-eager", Command: "must-not-start"}

	server, err := LaunchServerWithSandbox(
		context.Background(), &Registry{Servers: []ServerEntry{entry}}, entry.Name,
		tools.NewRegistry(), manager,
	)
	if server != nil || err == nil || !strings.Contains(err.Error(), sandbox.ErrManagerClosed.Error()) {
		t.Fatalf("closed sandbox launch = (%#v, %v), want nil manager-closed error", server, err)
	}
}

func TestLazyStdioSpawnRetainsSharedSandbox(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	entry := ServerEntry{Name: "closed-sandbox-lazy", Command: "must-not-start"}
	server := buildLazyServerWithSandbox(entry, entry, []CachedTool{{
		Name:        "probe",
		Description: "exercise lazy sandbox spawn",
		InputSchema: map[string]any{"type": "object"},
	}}, manager)
	defer server.Close()

	serverTools := server.Tools()
	if len(serverTools) != 1 {
		t.Fatalf("lazy server tools = %d, want 1", len(serverTools))
	}
	result, err := serverTools[0].Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("lazy tool execution returned transport error: %v", err)
	}
	if result == nil || !result.IsError || !strings.Contains(result.Output, sandbox.ErrManagerClosed.Error()) {
		t.Fatalf("lazy sandbox launch result = %#v, want manager-closed tool error", result)
	}
}

func TestRegistryHasEnabledServer(t *testing.T) {
	reg := &Registry{Servers: []ServerEntry{
		{Name: "disabled", Disabled: true},
		{Name: ReservedComputerUseName},
	}}
	if reg.HasEnabledServer("disabled") {
		t.Fatal("disabled server should not be reported as available")
	}
	if !reg.HasEnabledServer(ReservedComputerUseName) {
		t.Fatal("enabled computer-use server should be reported as available")
	}
	if reg.HasEnabledServer("missing") {
		t.Fatal("missing server should not be reported as available")
	}
}

// withTempHome scopes any Path() reads/writes to a fresh temp dir.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	return dir
}

func TestLoadMCP_MissingFileReturnsEmpty(t *testing.T) {
	withTempHome(t)
	r, err := Load()
	if err != nil {
		t.Fatalf("Load missing should not error: %v", err)
	}
	if len(r.Servers) != 0 {
		t.Errorf("missing file should yield empty servers; got %d", len(r.Servers))
	}
}

func TestAddSaveLoadRoundTrip(t *testing.T) {
	withTempHome(t)
	r := &Registry{}
	if err := AddServer(r, "fs", "mcp-fs", []string{"--root", "/tmp"}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := Save(r); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Servers) != 1 {
		t.Fatalf("len = %d, want 1", len(got.Servers))
	}
	s := got.Servers[0]
	if s.Name != "fs" || s.Command != "mcp-fs" {
		t.Errorf("got %+v", s)
	}
	if len(s.Args) != 2 || s.Args[0] != "--root" || s.Args[1] != "/tmp" {
		t.Errorf("args = %v", s.Args)
	}
}

func TestSaveMCP_FilePerm0600(t *testing.T) {
	withTempHome(t)
	if err := Save(&Registry{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := os.Stat(Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("mcp.toml perm = %o, want 600", mode)
	}
}

func TestAdd_RejectsEmpty(t *testing.T) {
	r := &Registry{}
	if err := AddServer(r, "", "x", nil); err == nil {
		t.Error("empty name should error")
	}
	if err := AddServer(r, "x", "", nil); err == nil {
		t.Error("empty command should error")
	}
}

func TestAddRejectsUnsafeServerNames(t *testing.T) {
	for _, name := range []string{
		"../escape", `..\\escape`, "has space", "line\nbreak", "中文", ".", "..", "trailing.",
		"CON", "con.json", "PRN", "AUX", "NUL", "COM1", "LPT9",
		strings.Repeat("a", maxMCPServerNameBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			reg := &Registry{}
			if err := AddServer(reg, name, "server", nil); err == nil {
				t.Fatalf("AddServer(%q) unexpectedly succeeded", name)
			}
			if len(reg.Servers) != 0 {
				t.Fatalf("AddServer(%q) mutated registry: %+v", name, reg.Servers)
			}
		})
	}
}

func TestAddAcceptsCommonASCIIServerNames(t *testing.T) {
	for _, name := range []string{"github", "github-enterprise_1.2", "-local", ".local", "_local"} {
		t.Run(name, func(t *testing.T) {
			if err := AddServer(&Registry{}, name, "server", nil); err != nil {
				t.Fatalf("AddServer(%q): %v", name, err)
			}
		})
	}
}

func TestLoadAndSaveRejectUnsafeConfiguredServerNames(t *testing.T) {
	home := withTempHome(t)
	unsafe := &Registry{Servers: []ServerEntry{{Name: "../escape", Command: "server"}}}
	if err := Save(unsafe); err == nil {
		t.Fatal("Save accepted an unsafe configured server name")
	}
	registryTOML := "[[servers]]\nname = \"../escape\"\ncommand = \"server\"\n"
	if err := os.WriteFile(filepath.Join(home, "mcp.toml"), []byte(registryTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted an unsafe configured server name")
	}
}

func TestLaunchAPIsRejectUnsafeConfiguredServerNames(t *testing.T) {
	withTempHome(t)
	entry := ServerEntry{Name: "../escape", Command: "definitely-not-executed"}
	reg := &Registry{Servers: []ServerEntry{entry}}

	if srv, err := LaunchServer(context.Background(), reg, entry.Name, tools.NewRegistry()); err == nil || srv != nil {
		if srv != nil {
			_ = srv.Close()
		}
		t.Fatalf("LaunchServer unsafe name = (%v, %v), want rejection", srv, err)
	}
	if servers, errs := LaunchAll(context.Background(), reg, tools.NewRegistry()); len(servers) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "invalid") {
		for _, srv := range servers {
			_ = srv.Close()
		}
		t.Fatalf("LaunchAll unsafe name = (%d servers, %v), want one validation error", len(servers), errs)
	}
	if servers, errs := LaunchAllLazy(context.Background(), reg, tools.NewRegistry(), LazyMCPModeAlways); len(servers) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "invalid") {
		for _, srv := range servers {
			_ = srv.Close()
		}
		t.Fatalf("LaunchAllLazy unsafe name = (%d servers, %v), want one validation error", len(servers), errs)
	}
}

func TestLaunchAllRejectsUnsafeNamesBeforeSkipFilters(t *testing.T) {
	for _, entry := range []ServerEntry{
		{Name: "../disabled", Command: "server", Disabled: true},
		{Name: "../empty"},
	} {
		t.Run(entry.Name, func(t *testing.T) {
			reg := &Registry{Servers: []ServerEntry{entry}}
			if servers, errs := LaunchAll(context.Background(), reg, tools.NewRegistry()); len(servers) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "invalid") {
				t.Fatalf("LaunchAll skipped unsafe entry: servers=%d errs=%v", len(servers), errs)
			}
			if servers, errs := LaunchAllLazy(context.Background(), reg, tools.NewRegistry(), LazyMCPModeAuto); len(servers) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "invalid") {
				t.Fatalf("LaunchAllLazy skipped unsafe entry: servers=%d errs=%v", len(servers), errs)
			}
		})
	}
}

func TestLaunchRejectsUnsafeServerNameMergedFromConfig(t *testing.T) {
	reg := &Registry{}
	reg.MergeWithConfig([]config.MCPServer{{
		Name:    "../from-config",
		Command: "definitely-not-executed",
	}})
	servers, errs := LaunchAll(context.Background(), reg, tools.NewRegistry())
	if len(servers) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "invalid") {
		for _, srv := range servers {
			_ = srv.Close()
		}
		t.Fatalf("merged unsafe config = (%d servers, %v), want one validation error", len(servers), errs)
	}
}

func TestLaunchAllLazySanitizesCredentialsEchoedByCurrentCache(t *testing.T) {
	withTempHome(t)
	const secret = "opaque-cache-credential-0123456789"
	entry := ServerEntry{
		Name:    "cached-safe",
		Command: "must-not-be-spawned-on-cache-hit",
		Env:     map[string]string{"FIRECRAWL_KEY": secret},
	}
	expanded, missing := expandEnvVarsInEntry(entry)
	if len(missing) != 0 {
		t.Fatalf("expand entry: missing=%v", missing)
	}
	cache := &Cache{
		Fingerprint: FingerprintEntry(expanded),
		Tools: []CachedTool{
			{
				Name:        "safe_tool",
				Description: "cached description echoed " + secret,
				InputSchema: map[string]any{
					"type":     "object",
					"required": []any{"credential_" + secret, "value"},
					"properties": map[string]any{
						"credential_" + secret: map[string]any{"type": "string"},
						"value":                map[string]any{"description": "schema echoed " + secret},
					},
				},
			},
			{Name: "unsafe_" + secret, Description: "must be dropped", InputSchema: map[string]any{"type": "object"}},
		},
	}
	if err := SaveCache(entry.Name, cache); err != nil {
		t.Fatal(err)
	}

	toolRegistry := tools.NewRegistry()
	servers, errs := LaunchAllLazy(
		context.Background(), &Registry{Servers: []ServerEntry{entry}}, toolRegistry, LazyMCPModeAuto,
	)
	if len(errs) != 0 || len(servers) != 1 {
		t.Fatalf("LaunchAllLazy = %d servers, errs=%v", len(servers), errs)
	}
	defer servers[0].Close()
	registered := toolRegistry.All()
	if len(registered) != 1 {
		t.Fatalf("registered cached tools = %d, want only the safe identifier", len(registered))
	}
	tool := registered[0]
	schema, _ := json.Marshal(tool.InputSchema())
	for label, value := range map[string]string{
		"name": tool.Name(), "description": tool.Description(), "schema": string(schema),
	} {
		if strings.Contains(value, secret) {
			t.Fatalf("cached %s leaked configured credential: %q", label, value)
		}
	}
	if !strings.Contains(tool.Description(), "[REDACTED]") || !strings.Contains(string(schema), "[REDACTED]") {
		t.Fatalf("cached metadata did not mark redaction: description=%q schema=%s", tool.Description(), schema)
	}
}

func TestAdd_ReplacesExisting(t *testing.T) {
	r := &Registry{}
	_ = AddServer(r, "fs", "old-bin", []string{"--v1"})
	_ = AddServer(r, "fs", "new-bin", []string{"--v2"})
	if len(r.Servers) != 1 {
		t.Fatalf("re-add should replace; len = %d", len(r.Servers))
	}
	if r.Servers[0].Command != "new-bin" || r.Servers[0].Args[0] != "--v2" {
		t.Errorf("replacement didn't apply: %+v", r.Servers[0])
	}
}

func TestRemoveMCPServer(t *testing.T) {
	r := &Registry{}
	_ = AddServer(r, "a", "x", nil)
	_ = AddServer(r, "b", "y", nil)

	if !RemoveServer(r, "a") {
		t.Error("Remove existing should return true")
	}
	if len(r.Servers) != 1 || r.Servers[0].Name != "b" {
		t.Errorf("after remove, expected only 'b'; got %+v", r.Servers)
	}
	if RemoveServer(r, "missing") {
		t.Error("Remove of non-existent should return false")
	}
}

func TestFindMCPServer(t *testing.T) {
	r := &Registry{}
	_ = AddServer(r, "fs", "mcp-fs", nil)
	if got := FindServer(r, "fs"); got == nil || got.Name != "fs" {
		t.Errorf("Find existing failed: %+v", got)
	}
	if got := FindServer(r, "missing"); got != nil {
		t.Errorf("Find missing should return nil; got %+v", got)
	}
}

func TestMergeWithConfig_DoesNotClobberMCPToml(t *testing.T) {
	r := &Registry{}
	_ = AddServer(r, "fs", "user-bin", []string{"--user"})

	cfgServers := []config.MCPServer{
		{Name: "fs", Command: "config-bin", Args: []string{"--config"}},
		{Name: "browser", Command: "browser-bin"},
	}
	r.MergeWithConfig(cfgServers)

	// fs should still point at user-bin (mcp.toml wins)
	fs := FindServer(r, "fs")
	if fs == nil || fs.Command != "user-bin" {
		t.Errorf("config clobbered runtime entry; got %+v", fs)
	}
	// browser was new — should be appended
	br := FindServer(r, "browser")
	if br == nil || br.Command != "browser-bin" {
		t.Errorf("config-only entry not merged; got %+v", br)
	}
}

func TestSavePath(t *testing.T) {
	dir := withTempHome(t)
	want := filepath.Join(dir, "mcp.toml")
	if got := Path(); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}
