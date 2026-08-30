package tui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
	"github.com/Ricardo-M-L/metis/pkg/tool"
)

func seedOAuthMCP(t *testing.T, entry mcp.ServerEntry) {
	t.Helper()
	if err := mcp.Save(&mcp.Registry{Servers: []mcp.ServerEntry{entry}}); err != nil {
		t.Fatalf("seed OAuth MCP entry: %v", err)
	}
}

func withTempMCPLoginHome(t *testing.T) {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
}

func stubMCPTokenEnsure(t *testing.T, stub func(context.Context, string, string, bool) (string, error)) {
	t.Helper()
	prior := ensureMCPToken
	ensureMCPToken = stub
	t.Cleanup(func() { ensureMCPToken = prior })
}

func stubMCPLoginStart(t *testing.T, stub func(context.Context, context.Context, string, *tools.Registry) (mcpLoginLaunch, error)) {
	t.Helper()
	prior := startMCPServerAfterLogin
	startMCPServerAfterLogin = stub
	t.Cleanup(func() { startMCPServerAfterLogin = prior })
}

func stubMCPServerLaunch(t *testing.T, stub func(context.Context, *mcp.Registry, string, *tools.Registry) (mcpLoginServer, error)) {
	t.Helper()
	prior := launchMCPServerAfterLogin
	launchMCPServerAfterLogin = stub
	t.Cleanup(func() { launchMCPServerAfterLogin = prior })
}

func TestMCPLoginExplicitlyInvokesInteractiveOAuth(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})

	called := 0
	stubMCPTokenEnsure(t, func(ctx context.Context, name, serverURL string, interactive bool) (string, error) {
		called++
		if ctx == nil {
			t.Error("OAuth ensure received a nil context")
		}
		if name != "secure" || serverURL != "https://mcp.example.test/api" {
			t.Errorf("OAuth ensure received name=%q url=%q", name, serverURL)
		}
		if !interactive {
			t.Error("explicit /mcp login did not enable the interactive OAuth flow")
		}
		return "secret-token-must-not-be-rendered", nil
	})

	command := BuildREPLCommands().Get("mcp")
	if command == nil {
		t.Fatal("/mcp command is not registered")
	}
	out := command.Handler(nil, "login secure")
	if called != 1 {
		t.Fatalf("interactive OAuth ensure calls = %d, want 1", called)
	}
	if !strings.Contains(out, "OAuth login complete") || !strings.Contains(out, "secure") {
		t.Fatalf("unexpected /mcp login success output: %q", out)
	}
	if strings.Contains(out, "secret-token-must-not-be-rendered") {
		t.Fatalf("/mcp login rendered the bearer token: %q", out)
	}
}

func TestMCPLoginValidatesConfiguredServerBeforeOAuth(t *testing.T) {
	tests := []struct {
		name  string
		entry *mcp.ServerEntry
		args  string
		want  string
	}{
		{name: "missing name", args: "login", want: "usage: mcp login <name>"},
		{name: "unknown server", args: "login ghost", want: "no MCP server named"},
		{
			name:  "stdio server",
			entry: &mcp.ServerEntry{Name: "local", Command: "example-mcp", Auth: "oauth"},
			args:  "login local",
			want:  "not an HTTP server",
		},
		{
			name:  "HTTP server without OAuth",
			entry: &mcp.ServerEntry{Name: "static", URL: "https://mcp.example.test", Headers: map[string]string{"Authorization": "ApiKey fixed"}},
			args:  "login static",
			want:  "does not use OAuth",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTempMCPLoginHome(t)
			if tt.entry != nil {
				seedOAuthMCP(t, *tt.entry)
			}
			called := 0
			stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
				called++
				return "unexpected", nil
			})

			out := cmdMCP(nil, tt.args)
			if !strings.Contains(out, tt.want) {
				t.Fatalf("cmdMCP(%q) = %q, want substring %q", tt.args, out, tt.want)
			}
			if called != 0 {
				t.Fatalf("invalid /mcp login invoked OAuth ensure %d time(s)", called)
			}
		})
	}
}

func TestMCPLoginSurfacesOAuthFailure(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
		return "", errors.New("authorization denied: Authorization: Bearer opaque-secret-token-123456")
	})

	out := cmdMCP(nil, "login secure")
	if !strings.Contains(out, "mcp login secure:") || !strings.Contains(out, "authorization denied") {
		t.Fatalf("unexpected /mcp login failure output: %q", out)
	}
	if strings.Contains(out, "opaque-secret-token") || !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("/mcp login failure did not redact bearer credential: %q", out)
	}
}

func TestMCPLoginExpandsServerURLLikeRuntimeLaunch(t *testing.T) {
	withTempMCPLoginHome(t)
	t.Setenv("METIS_MCP_LOGIN_HOST", "https://expanded.example.test")
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "${METIS_MCP_LOGIN_HOST}/api", Auth: "oauth",
	})
	stubMCPTokenEnsure(t, func(_ context.Context, name, serverURL string, interactive bool) (string, error) {
		if name != "secure" || serverURL != "https://expanded.example.test/api" || !interactive {
			t.Fatalf("OAuth ensure received name=%q url=%q interactive=%v", name, serverURL, interactive)
		}
		return "token", nil
	})

	out := cmdMCP(nil, "login secure")
	if !strings.Contains(out, "OAuth login complete") {
		t.Fatalf("unexpected /mcp login output: %q", out)
	}
}

func TestMCPLoginExpandsServerURLDefaultLikeRuntimeLaunch(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "${METIS_MCP_LOGIN_UNSET:-https://fallback.example.test}/api", Auth: "oauth",
	})
	stubMCPTokenEnsure(t, func(_ context.Context, _ string, serverURL string, _ bool) (string, error) {
		if serverURL != "https://fallback.example.test/api" {
			t.Fatalf("OAuth ensure URL = %q, want expanded fallback", serverURL)
		}
		return "token", nil
	})

	out := cmdMCP(nil, "login secure")
	if !strings.Contains(out, "OAuth login complete") {
		t.Fatalf("unexpected /mcp login output: %q", out)
	}
}

func TestMCPLoginRejectsUnsetServerURLEnvironment(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "${METIS_MCP_LOGIN_MISSING}/api", Auth: "oauth",
	})
	called := 0
	stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
		called++
		return "unexpected", nil
	})

	out := cmdMCP(nil, "login secure")
	for _, want := range []string{"references unset env vars", "METIS_MCP_LOGIN_MISSING", "${VAR:-default}"} {
		if !strings.Contains(out, want) {
			t.Fatalf("/mcp login missing-env output %q does not contain %q", out, want)
		}
	}
	if called != 0 {
		t.Fatalf("missing-env /mcp login invoked OAuth %d time(s)", called)
	}
}

func TestMCPNonLoginCommandsDoNotInvokeInteractiveOAuth(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	called := 0
	stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
		called++
		return "unexpected", nil
	})

	_ = cmdMCP(nil, "list")
	if called != 0 {
		t.Fatalf("/mcp list unexpectedly invoked interactive OAuth %d time(s)", called)
	}
}

func TestMCPLoginTUISubmitIsAsyncCancelable(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	m := newSlashTestModel(t)
	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	m.ctx = base
	started := make(chan struct{}, 1)
	stubMCPTokenEnsure(t, func(ctx context.Context, _ string, _ string, _ bool) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	})
	stubMCPLoginStart(t, func(context.Context, context.Context, string, *tools.Registry) (mcpLoginLaunch, error) {
		t.Fatal("canceled OAuth must not start the MCP server")
		return mcpLoginLaunch{}, nil
	})

	m.input.SetValue("/mcp login secure")
	begin := time.Now()
	cmd := pressEnter(t, m)
	if elapsed := time.Since(begin); elapsed > 200*time.Millisecond {
		t.Fatalf("/mcp login blocked Bubble Tea Update for %s", elapsed)
	}
	if cmd == nil || !m.mcpLoginPending {
		t.Fatalf("/mcp login did not return async cmd/pending state: cmd=%v pending=%v", cmd, m.mcpLoginPending)
	}
	select {
	case <-started:
		t.Fatal("OAuth runner started synchronously in Update")
	default:
	}

	resultCh := make(chan mcpLoginResultMsg, 1)
	go func() { resultCh <- cmd().(mcpLoginResultMsg) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async OAuth runner did not start")
	}
	_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	select {
	case result := <-resultCh:
		_, _ = m.Update(result)
	case <-time.After(time.Second):
		t.Fatal("Esc did not cancel OAuth")
	}
	if m.mcpLoginPending || m.mcpLoginCancel != nil {
		t.Fatalf("OAuth cancellation left pending state: pending=%v cancel=%v", m.mcpLoginPending, m.mcpLoginCancel != nil)
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "warning" || !strings.Contains(strings.ToLower(last.Content), "cancel") {
		t.Fatalf("OAuth cancellation result = %+v", last)
	}
}

func TestMCPLoginEscCancelsOAuthAndForegroundTurnTogether(t *testing.T) {
	m := newSlashTestModel(t)
	oauthCanceled := 0
	turnCanceled := 0
	m.mcpLoginPending = true
	m.mcpLoginCancel = func() { oauthCanceled++ }
	m.turnActive = true
	m.turnCancel = func() { turnCanceled++ }
	m.spinnerActive = true

	updated, cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if updated != m || cmd != nil {
		t.Fatalf("Esc result model=%T cmd=%v", updated, cmd)
	}
	if oauthCanceled != 1 || turnCanceled != 1 {
		t.Fatalf("Esc cancellations: oauth=%d turn=%d, want 1/1", oauthCanceled, turnCanceled)
	}
	if m.mcpLoginCancel != nil || m.turnCancel != nil || m.spinnerActive {
		t.Fatalf("Esc left cancellation state: oauth=%v turn=%v spinner=%v", m.mcpLoginCancel != nil, m.turnCancel != nil, m.spinnerActive)
	}
}

func TestMCPLoginTUIModelContextCancellationStopsOAuth(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	m := newSlashTestModel(t)
	base, cancelBase := context.WithCancel(context.Background())
	m.ctx = base
	started := make(chan struct{}, 1)
	stubMCPTokenEnsure(t, func(ctx context.Context, _ string, _ string, _ bool) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	})
	stubMCPLoginStart(t, func(context.Context, context.Context, string, *tools.Registry) (mcpLoginLaunch, error) {
		t.Fatal("canceled Model context must not start the MCP server")
		return mcpLoginLaunch{}, nil
	})

	m.input.SetValue("/mcp login secure")
	cmd := pressEnter(t, m)
	resultCh := make(chan mcpLoginResultMsg, 1)
	go func() { resultCh <- cmd().(mcpLoginResultMsg) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("OAuth did not start")
	}
	cancelBase()
	select {
	case result := <-resultCh:
		_, _ = m.Update(result)
	case <-time.After(time.Second):
		t.Fatal("canceling Model context did not stop OAuth")
	}
	if m.mcpLoginPending || m.mcpLoginCancel != nil {
		t.Fatal("Model cancellation left MCP login pending")
	}
}

type mcpLoginTestTool struct{ tool.BaseTool }

type mcpLoginTestServer struct{ closed atomic.Bool }

func (s *mcpLoginTestServer) Close() error {
	s.closed.Store(true)
	return nil
}

func (mcpLoginTestTool) Name() string                { return "mcp__secure__ping" }
func (mcpLoginTestTool) Description() string         { return "test" }
func (mcpLoginTestTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (mcpLoginTestTool) Concurrency(map[string]any) tool.Concurrency {
	return tool.ConcurrencySafe
}
func (mcpLoginTestTool) CanUse(context.Context, map[string]any) (tool.Permission, string) {
	return tool.PermissionAllow, ""
}
func (mcpLoginTestTool) Execute(context.Context, map[string]any) (*tool.Result, error) {
	return &tool.Result{Output: "pong"}, nil
}

func TestMCPLoginTUISuccessRegistersToolsBeforeResult(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	m := newSlashTestModel(t)
	stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
		return "secret-token-must-not-be-rendered", nil
	})
	var operationCtx, lifecycleCtx context.Context
	testServer := &mcpLoginTestServer{}
	stubMCPLoginStart(t, func(opCtx context.Context, liveCtx context.Context, name string, _ *tools.Registry) (mcpLoginLaunch, error) {
		operationCtx, lifecycleCtx = opCtx, liveCtx
		if name != "secure" {
			t.Fatalf("start name = %q", name)
		}
		return mcpLoginLaunch{server: testServer, tools: []tools.Tool{mcpLoginTestTool{}}}, nil
	})

	m.input.SetValue("/mcp login secure")
	msg := runCmd(t, pressEnter(t, m))
	if _, ok := m.loop.Registry.Get("mcp__secure__ping"); ok {
		t.Fatal("tea.Cmd mutated the live registry before Update handled its result")
	}
	_, _ = m.Update(msg)
	if _, ok := m.loop.Registry.Get("mcp__secure__ping"); !ok {
		t.Fatal("Update did not register the authenticated MCP tool for the current session")
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != "success" || !strings.Contains(last.Content, "1 tool") {
		t.Fatalf("OAuth success result = %+v", last)
	}
	if strings.Contains(last.Content, "secret-token-must-not-be-rendered") {
		t.Fatalf("OAuth success rendered token: %q", last.Content)
	}
	if operationCtx == nil || operationCtx.Err() == nil {
		t.Fatal("completed login operation context was not released")
	}
	if lifecycleCtx == nil || lifecycleCtx.Err() != nil {
		t.Fatalf("successful live MCP inherited the short login context: %v", lifecycleCtx)
	}
	if len(m.mcpLoginServers) != 1 || testServer.closed.Load() {
		t.Fatalf("Model did not retain the live MCP server: owners=%d closed=%v", len(m.mcpLoginServers), testServer.closed.Load())
	}
	if err := m.closeMCPLoginServers(); err != nil {
		t.Fatalf("close Model MCP servers: %v", err)
	}
	if !testServer.closed.Load() || len(m.mcpLoginServers) != 0 {
		t.Fatalf("Model MCP cleanup failed: closed=%v owners=%d", testServer.closed.Load(), len(m.mcpLoginServers))
	}
}

func TestMCPLoginReconnectsWhenServerToolsAlreadyExist(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	registry := tools.NewRegistry()
	registry.Register(mcpLoginTestTool{})
	called := 0
	testServer := &mcpLoginTestServer{}
	stubMCPServerLaunch(t, func(context.Context, *mcp.Registry, string, *tools.Registry) (mcpLoginServer, error) {
		called++
		return testServer, nil
	})

	launch, err := startMCPServerAfterLogin(context.Background(), context.Background(), "secure", registry)
	if err != nil {
		t.Fatalf("start explicit reconnect: %v", err)
	}
	defer closeMCPLoginLaunch(launch)
	if called != 1 {
		t.Fatalf("explicit reconnect launch calls = %d, want 1", called)
	}
}

func TestMCPLoginClosesPartialServerReturnedWithLaunchError(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	testServer := &mcpLoginTestServer{}
	stubMCPServerLaunch(t, func(context.Context, *mcp.Registry, string, *tools.Registry) (mcpLoginServer, error) {
		return testServer, errors.New("handshake failed after allocation")
	})

	_, err := startMCPServerAfterLogin(context.Background(), context.Background(), "secure", tools.NewRegistry())
	if err == nil {
		t.Fatal("partial launch unexpectedly succeeded")
	}
	if !testServer.closed.Load() {
		t.Fatal("partial server returned with an error was not closed")
	}
}

func TestMCPLoginLiveStartRevalidatesOAuthHTTPEntry(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{Name: "secure", Command: "must-not-run"})
	called := 0
	stubMCPServerLaunch(t, func(context.Context, *mcp.Registry, string, *tools.Registry) (mcpLoginServer, error) {
		called++
		return &mcpLoginTestServer{}, nil
	})

	_, err := startMCPServerAfterLogin(context.Background(), context.Background(), "secure", tools.NewRegistry())
	if err == nil || !strings.Contains(err.Error(), "no longer an OAuth HTTP server") {
		t.Fatalf("changed login target error = %v", err)
	}
	if called != 0 {
		t.Fatalf("changed login target launched %d server(s)", called)
	}
}

func TestMCPLoginKeepsResourceOnlyServerAlive(t *testing.T) {
	registry := tools.NewRegistry()
	testServer := &mcpLoginTestServer{}
	count, owns := publishMCPLoginLaunch(registry, "secure", mcpLoginLaunch{server: testServer})
	if count != 0 || !owns {
		t.Fatalf("resource-only publication = count %d owns %v, want 0/true", count, owns)
	}
	if testServer.closed.Load() {
		t.Fatal("resource/prompt-only server was closed during publication")
	}
	_ = testServer.Close()
}

func TestMCPLoginTransfersConcreteResourceOnlyServerToRuntime(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	m := newSlashTestModel(t)
	stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
		return "mock-token", nil
	})
	server := mcptools.NewLazyServer("secure", nil, nil)
	stubMCPLoginStart(t, func(context.Context, context.Context, string, *tools.Registry) (mcpLoginLaunch, error) {
		return mcpLoginLaunch{server: server}, nil
	})
	adopted := 0
	m.ext.AdoptMCPServer = func(got *mcptools.Server, discovered []tools.Tool) bool {
		adopted++
		if got != server || len(discovered) != 0 {
			t.Fatalf("adoption got server=%p tools=%d", got, len(discovered))
		}
		return true
	}

	m.input.SetValue("/mcp login secure")
	msg := runCmd(t, pressEnter(t, m))
	_, _ = m.Update(msg)
	if adopted != 1 {
		t.Fatalf("runtime adoption calls = %d, want 1", adopted)
	}
	if len(m.mcpLoginServers) != 0 {
		t.Fatalf("runtime-owned server was also retained by Model: %d", len(m.mcpLoginServers))
	}
}

type orderedMCPLoginTestServer struct {
	turnStopped *atomic.Bool
	closed      atomic.Bool
	closedEarly atomic.Bool
}

func (s *orderedMCPLoginTestServer) Close() error {
	if !s.turnStopped.Load() {
		s.closedEarly.Store(true)
	}
	s.closed.Store(true)
	return nil
}

func TestShutdownTUILiveWorkStopsTurnBeforeClosingMCP(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	m.doneCh = make(chan error, 1)
	var turnStopped atomic.Bool
	m.turnCancel = func() {
		turnStopped.Store(true)
		m.doneCh <- context.Canceled
	}
	server := &orderedMCPLoginTestServer{turnStopped: &turnStopped}
	m.mcpLoginServers = []mcpLoginServer{server}

	turnOK, err := shutdownTUILiveWork(m)
	if err != nil {
		t.Fatalf("shutdown live work: %v", err)
	}
	if !turnOK || !server.closed.Load() || server.closedEarly.Load() {
		t.Fatalf("shutdown order: turnOK=%v closed=%v closedEarly=%v", turnOK, server.closed.Load(), server.closedEarly.Load())
	}
}

func TestMCPLoginTUICancelDuringLiveStartReturnsWithoutCancelingSession(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	m := newSlashTestModel(t)
	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	m.ctx = base
	stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
		return "token", nil
	})
	startReached := make(chan struct{}, 1)
	stubMCPLoginStart(t, func(opCtx, liveCtx context.Context, _ string, _ *tools.Registry) (mcpLoginLaunch, error) {
		if liveCtx != base {
			t.Errorf("live start context is not the Model lifecycle context")
		}
		startReached <- struct{}{}
		<-opCtx.Done()
		return mcpLoginLaunch{}, opCtx.Err()
	})

	m.input.SetValue("/mcp login secure")
	cmd := pressEnter(t, m)
	resultCh := make(chan mcpLoginResultMsg, 1)
	go func() { resultCh <- cmd().(mcpLoginResultMsg) }()
	select {
	case <-startReached:
	case <-time.After(time.Second):
		t.Fatal("login did not reach live MCP start")
	}
	_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	select {
	case result := <-resultCh:
		_, _ = m.Update(result)
	case <-time.After(time.Second):
		t.Fatal("Esc did not cancel live MCP startup")
	}
	if base.Err() != nil {
		t.Fatalf("canceling login canceled the Model lifecycle: %v", base.Err())
	}
	if m.mcpLoginPending || m.mcpLoginCancel != nil {
		t.Fatal("live-start cancellation left MCP login pending")
	}
}

func TestMCPLoginPlainREPLUsesBoundedContext(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	m := newSlashTestModel(t)
	r := m.asREPL()
	stubMCPTokenEnsure(t, func(ctx context.Context, _ string, _ string, _ bool) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > mcpLoginTimeout {
			t.Fatalf("plain REPL OAuth context is not bounded: deadline=%v ok=%v", deadline, ok)
		}
		return "token", nil
	})
	testServer := &mcpLoginTestServer{}
	stubMCPLoginStart(t, func(context.Context, context.Context, string, *tools.Registry) (mcpLoginLaunch, error) {
		return mcpLoginLaunch{server: testServer, tools: []tools.Tool{mcpLoginTestTool{}}}, nil
	})

	out := r.handleMCPLogin("secure")
	if !strings.Contains(out, "OAuth login complete") {
		t.Fatalf("plain REPL OAuth output = %q", out)
	}
	if len(r.mcpLoginServers) != 1 || testServer.closed.Load() {
		t.Fatalf("plain REPL did not retain live MCP server: owners=%d closed=%v", len(r.mcpLoginServers), testServer.closed.Load())
	}
	if err := r.closeMCPLoginServers(); err != nil {
		t.Fatalf("close REPL MCP servers: %v", err)
	}
	if !testServer.closed.Load() || len(r.mcpLoginServers) != 0 {
		t.Fatalf("plain REPL MCP cleanup failed: closed=%v owners=%d", testServer.closed.Load(), len(r.mcpLoginServers))
	}
}

func TestMCPLoginPlainREPLRunClosesLiveServer(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	m := newSlashTestModel(t)
	r := m.asREPL()
	r.Styles = NewStyles()
	r.stdin = strings.NewReader("/mcp login secure\n/quit\n")
	r.out = &bytes.Buffer{}
	stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
		return "token", nil
	})
	testServer := &mcpLoginTestServer{}
	stubMCPLoginStart(t, func(context.Context, context.Context, string, *tools.Registry) (mcpLoginLaunch, error) {
		return mcpLoginLaunch{server: testServer, tools: []tools.Tool{mcpLoginTestTool{}}}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatalf("plain REPL Run: %v", err)
	}
	if !testServer.closed.Load() || len(r.mcpLoginServers) != 0 {
		t.Fatalf("REPL.Run did not close login server: closed=%v owners=%d", testServer.closed.Load(), len(r.mcpLoginServers))
	}
}

func TestMCPLoginOperationCancellationReachesLaunchServer(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	operationCtx, cancelOperation := context.WithCancel(context.Background())
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	defer cancelLifecycle()
	launchCanceled := make(chan struct{})
	stubMCPServerLaunch(t, func(ctx context.Context, _ *mcp.Registry, _ string, _ *tools.Registry) (mcpLoginServer, error) {
		<-ctx.Done()
		close(launchCanceled)
		return nil, ctx.Err()
	})

	resultCh := make(chan error, 1)
	go func() {
		_, err := startMCPServerAfterLogin(operationCtx, lifecycleCtx, "secure", tools.NewRegistry())
		resultCh <- err
	}()
	cancelOperation()
	select {
	case <-launchCanceled:
	case <-time.After(time.Second):
		t.Fatal("operation cancellation did not reach LaunchServer context")
	}
	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("start after operation cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("operation cancellation did not release live-start caller")
	}
}

func TestMCPLoginSuccessfulLaunchHandsContextToLifecycle(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	operationCtx, cancelOperation := context.WithCancel(context.Background())
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	var launchCtx context.Context
	testServer := &mcpLoginTestServer{}
	stubMCPServerLaunch(t, func(ctx context.Context, _ *mcp.Registry, _ string, _ *tools.Registry) (mcpLoginServer, error) {
		launchCtx = ctx
		return testServer, nil
	})

	launch, err := startMCPServerAfterLogin(operationCtx, lifecycleCtx, "secure", tools.NewRegistry())
	if err != nil {
		t.Fatalf("start MCP server: %v", err)
	}
	defer closeMCPLoginLaunch(launch)
	cancelOperation()
	select {
	case <-launchCtx.Done():
		t.Fatalf("successful server remained bound to operation context: %v", launchCtx.Err())
	case <-time.After(20 * time.Millisecond):
	}
	cancelLifecycle()
	select {
	case <-launchCtx.Done():
		if !errors.Is(launchCtx.Err(), context.Canceled) {
			t.Fatalf("lifecycle cancellation error = %v", launchCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("successful server did not remain bound to lifecycle context")
	}
}

func TestMCPStartSuccessfulLaunchHandsContextToLifecycle(t *testing.T) {
	operationCtx, cancelOperation := context.WithCancel(context.Background())
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	var launchCtx context.Context
	server := mcptools.NewLazyServer("secure", nil, nil)

	got, err := launchMCPServerWithLifecycle(operationCtx, lifecycleCtx, func(ctx context.Context) (*mcptools.Server, error) {
		launchCtx = ctx
		return server, nil
	})
	if err != nil || got != server {
		t.Fatalf("start MCP server = %p, %v", got, err)
	}
	cancelOperation()
	select {
	case <-launchCtx.Done():
		t.Fatalf("successful /mcp start remained bound to operation context: %v", launchCtx.Err())
	case <-time.After(20 * time.Millisecond):
	}
	cancelLifecycle()
	select {
	case <-launchCtx.Done():
		if !errors.Is(launchCtx.Err(), context.Canceled) {
			t.Fatalf("lifecycle cancellation error = %v", launchCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("successful /mcp start did not remain bound to lifecycle context")
	}
	_ = server.Close()
}

func TestMCPLoginTUICancelClosesLateSuccessWithoutPublishing(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	m := newSlashTestModel(t)
	stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
		return "token", nil
	})
	reached := make(chan struct{})
	release := make(chan struct{})
	testServer := &mcpLoginTestServer{}
	stubMCPLoginStart(t, func(context.Context, context.Context, string, *tools.Registry) (mcpLoginLaunch, error) {
		close(reached)
		<-release // Deliberately emulate a launcher that notices cancellation late.
		return mcpLoginLaunch{server: testServer, tools: []tools.Tool{mcpLoginTestTool{}}}, nil
	})

	m.input.SetValue("/mcp login secure")
	cmd := pressEnter(t, m)
	resultCh := make(chan mcpLoginResultMsg, 1)
	go func() { resultCh <- cmd().(mcpLoginResultMsg) }()
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("login did not reach late launcher")
	}
	if !m.cancelMCPLogin() {
		t.Fatal("pending login was not cancelable")
	}
	close(release)
	var result mcpLoginResultMsg
	select {
	case result = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("late launcher did not return")
	}
	_, _ = m.Update(result)
	if !testServer.closed.Load() {
		t.Fatal("late successful server was not closed after cancellation")
	}
	if _, ok := m.loop.Registry.Get("mcp__secure__ping"); ok {
		t.Fatal("tools from a closed late-success server were published")
	}
}

func TestMCPLoginTUIShutdownClosesUnconsumedSuccess(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	m := newSlashTestModel(t)
	stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
		return "token", nil
	})
	testServer := &mcpLoginTestServer{}
	stubMCPLoginStart(t, func(context.Context, context.Context, string, *tools.Registry) (mcpLoginLaunch, error) {
		return mcpLoginLaunch{server: testServer, tools: []tools.Tool{mcpLoginTestTool{}}}, nil
	})

	m.input.SetValue("/mcp login secure")
	result := pressEnter(t, m)().(mcpLoginResultMsg)
	if testServer.closed.Load() {
		t.Fatal("server closed before pending result ownership was canceled")
	}
	if !m.cancelMCPLogin() {
		t.Fatal("shutdown could not abort pending result ownership")
	}
	if !testServer.closed.Load() {
		t.Fatal("unconsumed successful server leaked across TUI shutdown")
	}
	_, _ = m.Update(result)
	if _, ok := m.loop.Registry.Get("mcp__secure__ping"); ok {
		t.Fatal("closed unconsumed server was published by a late result")
	}
}

func TestMCPLoginPlainREPLCancellationBeforePublishClosesServer(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	m := newSlashTestModel(t)
	r := m.asREPL()
	operationCtx, cancelOperation := context.WithCancel(context.Background())
	defer cancelOperation()
	stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
		return "token", nil
	})
	testServer := &mcpLoginTestServer{}
	stubMCPLoginStart(t, func(context.Context, context.Context, string, *tools.Registry) (mcpLoginLaunch, error) {
		cancelOperation()
		return mcpLoginLaunch{server: testServer, tools: []tools.Tool{mcpLoginTestTool{}}}, nil
	})

	out := r.handleMCPLoginContext(operationCtx, context.Background(), "secure")
	if !strings.Contains(strings.ToLower(out), "canceled") {
		t.Fatalf("REPL cancellation output = %q", out)
	}
	if !testServer.closed.Load() {
		t.Fatal("REPL cancellation leaked a server returned immediately before publish")
	}
	if _, ok := r.Loop.Registry.Get("mcp__secure__ping"); ok {
		t.Fatal("REPL published tools after operation cancellation")
	}
	if len(r.mcpLoginServers) != 0 {
		t.Fatalf("REPL retained canceled server ownership: %d", len(r.mcpLoginServers))
	}
}
