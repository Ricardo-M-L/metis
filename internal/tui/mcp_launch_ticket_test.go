package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

type explicitLaunchTicketHarness struct {
	epoch       uint64
	beginCalls  int
	adoptCalls  int
	legacyCalls int
	finishCalls int
}

func (h *explicitLaunchTicketHarness) begin(ctx context.Context) *MCPLaunchTicket {
	h.beginCalls++
	captured := h.epoch
	adopt := func(server *mcptools.Server, _ []tools.Tool) bool {
		h.adoptCalls++
		if captured != h.epoch {
			_ = server.Close()
			return true
		}
		return false
	}
	return NewMCPLaunchTicket(ctx, adopt, func() { h.finishCalls++ })
}

func (h *explicitLaunchTicketHarness) legacy(*mcptools.Server, []tools.Tool) bool {
	h.legacyCalls++
	return false
}

func explicitTicketTestServer(name string) *mcptools.Server {
	return mcptools.NewLazyServer(name, []mcpsdk.Tool{{Name: "ping"}}, func(context.Context) (*mcpsdk.Client, error) {
		return nil, errors.New("closed server unexpectedly attempted a lazy spawn")
	})
}

func assertStaleExplicitLaunchRejected(t *testing.T, registry *tools.Registry, server *mcptools.Server, h *explicitLaunchTicketHarness) {
	t.Helper()
	if h.beginCalls != 1 || h.adoptCalls != 1 || h.legacyCalls != 0 || h.finishCalls != 1 {
		t.Fatalf("launch hook calls = begin:%d adopt:%d legacy:%d finish:%d, want 1/1/0/1", h.beginCalls, h.adoptCalls, h.legacyCalls, h.finishCalls)
	}
	if _, ok := registry.Get("mcp__" + server.Name() + "__ping"); ok {
		t.Fatal("stale explicit launch published its tool namespace")
	}
	result, err := server.Tools()[0].Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("probe rejected server: %v", err)
	}
	if result == nil || !result.IsError || !strings.Contains(strings.ToLower(result.Output), "closed") {
		t.Fatalf("stale explicit launch was not closed: %#v", result)
	}
}

func stubConfiguredMCPLaunch(t *testing.T, stub func(context.Context, *mcp.Registry, string, *tools.Registry, *sandbox.Manager) (*mcptools.Server, error)) {
	t.Helper()
	prior := launchConfiguredMCPServer
	launchConfiguredMCPServer = stub
	t.Cleanup(func() { launchConfiguredMCPServer = prior })
}

func TestExplicitMCPEntryPointsRetainLaunchTicketAcrossRevocation(t *testing.T) {
	t.Run("login", func(t *testing.T) {
		withTempMCPLoginHome(t)
		seedOAuthMCP(t, mcp.ServerEntry{
			Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
		})
		m := newSlashTestModel(t)
		h := &explicitLaunchTicketHarness{epoch: 7}
		m.ext.BeginMCPLaunch = h.begin
		m.ext.AdoptMCPServer = h.legacy
		stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
			return "token", nil
		})
		server := explicitTicketTestServer("secure")
		stubMCPLoginStart(t, func(context.Context, context.Context, string, *tools.Registry) (mcpLoginLaunch, error) {
			h.epoch++ // fullAccess -> safe revocation while the launch is in flight
			return mcpLoginLaunch{server: server, tools: server.Tools()}, nil
		})

		m.input.SetValue("/mcp login secure")
		msg := pressEnter(t, m)()
		_, _ = m.Update(msg)
		assertStaleExplicitLaunchRejected(t, m.loop.Registry, server, h)
	})

	t.Run("start", func(t *testing.T) {
		withTempMCPLoginHome(t)
		if err := mcp.Save(&mcp.Registry{Servers: []mcp.ServerEntry{{Name: "secure", Command: "unused"}}}); err != nil {
			t.Fatal(err)
		}
		m := newSlashTestModel(t)
		r := m.asREPL()
		h := &explicitLaunchTicketHarness{epoch: 11}
		r.BeginMCPLaunch = h.begin
		r.AdoptMCPServer = h.legacy
		server := explicitTicketTestServer("secure")
		stubConfiguredMCPLaunch(t, func(_ context.Context, _ *mcp.Registry, _ string, staged *tools.Registry, _ *sandbox.Manager) (*mcptools.Server, error) {
			for _, tool := range server.Tools() {
				staged.Register(tool)
			}
			h.epoch++
			return server, nil
		})

		_ = r.handleMCPStart("secure")
		assertStaleExplicitLaunchRejected(t, r.Loop.Registry, server, h)
	})

	t.Run("computer use", func(t *testing.T) {
		withTempCuEnv(t)
		m := newSlashTestModel(t)
		r := m.asREPL()
		h := &explicitLaunchTicketHarness{epoch: 19}
		r.BeginMCPLaunch = h.begin
		r.AdoptMCPServer = h.legacy
		server := explicitTicketTestServer(cuServerName)
		stubConfiguredMCPLaunch(t, func(_ context.Context, _ *mcp.Registry, _ string, staged *tools.Registry, _ *sandbox.Manager) (*mcptools.Server, error) {
			for _, tool := range server.Tools() {
				staged.Register(tool)
			}
			h.epoch++
			return server, nil
		})

		_ = cuEnable(r)
		assertStaleExplicitLaunchRejected(t, r.Loop.Registry, server, h)
	})

	t.Run("test probe", func(t *testing.T) {
		withTempMCPLoginHome(t)
		if err := mcp.Save(&mcp.Registry{Servers: []mcp.ServerEntry{{Name: "secure", Command: "unused"}}}); err != nil {
			t.Fatal(err)
		}
		m := newSlashTestModel(t)
		r := m.asREPL()
		h := &explicitLaunchTicketHarness{epoch: 23}
		r.BeginMCPLaunch = h.begin
		r.AdoptMCPServer = h.legacy
		server := explicitTicketTestServer("secure")
		stubConfiguredMCPLaunch(t, func(_ context.Context, _ *mcp.Registry, _ string, staged *tools.Registry, _ *sandbox.Manager) (*mcptools.Server, error) {
			for _, tool := range server.Tools() {
				staged.Register(tool)
			}
			h.epoch++
			return server, nil
		})

		_ = r.handleMCPTest("secure")
		if h.beginCalls != 1 || h.adoptCalls != 0 || h.legacyCalls != 0 || h.finishCalls != 1 {
			t.Fatalf("probe hook calls = begin:%d adopt:%d legacy:%d finish:%d, want 1/0/0/1", h.beginCalls, h.adoptCalls, h.legacyCalls, h.finishCalls)
		}
		if _, ok := r.Loop.Registry.Get("mcp__secure__ping"); ok {
			t.Fatal("one-shot MCP probe published tools into the live registry")
		}
		result, err := server.Tools()[0].Execute(context.Background(), nil)
		if err != nil || result == nil || !result.IsError || !strings.Contains(strings.ToLower(result.Output), "closed") {
			t.Fatalf("one-shot MCP probe did not close its server: result=%#v err=%v", result, err)
		}
	})
}

func TestMCPLoginTicketCancellationClosesStagedResultWithoutUpdate(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	m := newSlashTestModel(t)
	var revoke context.CancelFunc
	finished := make(chan struct{})
	m.ext.BeginMCPLaunch = func(parent context.Context) *MCPLaunchTicket {
		ctx, cancel := context.WithCancel(parent)
		revoke = cancel
		return NewMCPLaunchTicket(ctx, func(*mcptools.Server, []tools.Tool) bool {
			t.Fatal("staged result must be closed before adoption")
			return false
		}, func() { close(finished) })
	}
	stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
		return "token", nil
	})
	server := explicitTicketTestServer("secure")
	stubMCPLoginStart(t, func(context.Context, context.Context, string, *tools.Registry) (mcpLoginLaunch, error) {
		return mcpLoginLaunch{server: server, tools: server.Tools()}, nil
	})

	m.input.SetValue("/mcp login secure")
	result := pressEnter(t, m)().(mcpLoginResultMsg)
	if revoke == nil {
		t.Fatal("login did not capture a runtime launch ticket")
	}
	// The tea.Cmd has returned a successful staged server, but Bubble Tea has
	// not yet handled its result. Revocation must close/release it without
	// waiting for the UI goroutine, otherwise the runtime's strict join would
	// deadlock behind this pending message.
	revoke()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("ticket revocation waited for Bubble Tea Update to release the staged server")
	}
	probe, err := server.Tools()[0].Execute(context.Background(), nil)
	if err != nil || probe == nil || !probe.IsError || !strings.Contains(strings.ToLower(probe.Output), "closed") {
		t.Fatalf("revocation did not close staged login server: result=%#v err=%v", probe, err)
	}
	if _, ok := m.loop.Registry.Get("mcp__secure__ping"); ok {
		t.Fatal("revoked staged login result published tools")
	}
	// Consume the already-aborted result to restore Model bookkeeping; all
	// ownership actions are exactly-once no-ops at this point.
	m.handleMCPLoginResult(result)
}

func TestMCPLoginTicketCancellationBeforeCommandStartsReleasesLease(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	m := newSlashTestModel(t)
	finished := make(chan struct{})
	m.ext.BeginMCPLaunch = func(parent context.Context) *MCPLaunchTicket {
		ctx, cancel := context.WithCancel(parent)
		return NewMCPLaunchTicket(ctx, nil, func() {
			cancel()
			close(finished)
		})
	}
	oauthCalls := 0
	stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
		oauthCalls++
		return "unexpected", nil
	})
	launchCalls := 0
	stubMCPLoginStart(t, func(context.Context, context.Context, string, *tools.Registry) (mcpLoginLaunch, error) {
		launchCalls++
		return mcpLoginLaunch{}, nil
	})

	cmd := m.startMCPLogin("secure")
	if cmd == nil {
		t.Fatal("login did not create an asynchronous command")
	}
	// Simulate application shutdown after Update returns the Cmd but before
	// Bubble Tea schedules it. No producer exists, so cancel must release the
	// runtime join edge without waiting for a result message that cannot arrive.
	if !m.cancelMCPLogin() {
		t.Fatal("queued login was not cancelable")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("queued but unstarted login retained its runtime launch lease")
	}
	if oauthCalls != 0 || launchCalls != 0 {
		t.Fatalf("unstarted login invoked oauth=%d launcher=%d", oauthCalls, launchCalls)
	}

	// A scheduler racing just after cancellation must observe the aborted
	// lease and remain side-effect free.
	msg, ok := cmd().(mcpLoginResultMsg)
	if !ok || !errors.Is(msg.err, context.Canceled) {
		t.Fatalf("canceled queued command result = %#v, want context.Canceled", msg)
	}
	if oauthCalls != 0 || launchCalls != 0 {
		t.Fatalf("canceled queued command invoked oauth=%d launcher=%d", oauthCalls, launchCalls)
	}
}

func TestBlockingExplicitMCPEntryPointsJoinBeforeTicketFinish(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T)
		run   func(*REPL) string
	}{
		{
			name: "start",
			setup: func(t *testing.T) {
				withTempMCPLoginHome(t)
				if err := mcp.Save(&mcp.Registry{Servers: []mcp.ServerEntry{{Name: "secure", Command: "unused"}}}); err != nil {
					t.Fatal(err)
				}
			},
			run: func(r *REPL) string { return r.handleMCPStart("secure") },
		},
		{
			name: "test probe",
			setup: func(t *testing.T) {
				withTempMCPLoginHome(t)
				if err := mcp.Save(&mcp.Registry{Servers: []mcp.ServerEntry{{Name: "secure", Command: "unused"}}}); err != nil {
					t.Fatal(err)
				}
			},
			run: func(r *REPL) string { return r.handleMCPTest("secure") },
		},
		{
			name: "computer use",
			setup: func(t *testing.T) {
				withTempCuEnv(t)
			},
			run: cuEnable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup(t)
			m := newSlashTestModel(t)
			r := m.asREPL()
			var revoke context.CancelFunc
			finished := make(chan struct{})
			r.BeginMCPLaunch = func(parent context.Context) *MCPLaunchTicket {
				ctx, cancel := context.WithCancel(parent)
				revoke = cancel
				return NewMCPLaunchTicket(ctx, nil, func() { close(finished) })
			}
			started := make(chan struct{})
			canceled := make(chan struct{})
			release := make(chan struct{})
			stubConfiguredMCPLaunch(t, func(ctx context.Context, _ *mcp.Registry, _ string, _ *tools.Registry, _ *sandbox.Manager) (*mcptools.Server, error) {
				close(started)
				<-ctx.Done()
				close(canceled)
				<-release // cancellation-insensitive unwind tail
				return nil, ctx.Err()
			})
			returned := make(chan struct{})
			go func() {
				_ = tt.run(r)
				close(returned)
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("explicit MCP entry did not reach its launcher")
			}
			if revoke == nil {
				t.Fatal("explicit MCP entry did not register a launch ticket")
			}
			revoke()
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("ticket cancellation did not reach launcher context")
			}
			select {
			case <-finished:
				close(release)
				t.Fatal("ticket finished before the launcher unwind returned")
			default:
			}
			close(release)
			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Fatal("explicit MCP entry did not return after launcher unwind")
			}
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("explicit MCP entry returned without releasing its ticket")
			}
		})
	}
}

func TestRejectedMCPLaunchTicketDoesNotCallLauncher(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T)
		run   func(*REPL) string
	}{
		{
			name: "start",
			setup: func(t *testing.T) {
				withTempMCPLoginHome(t)
				if err := mcp.Save(&mcp.Registry{Servers: []mcp.ServerEntry{{Name: "secure", Command: "unused"}}}); err != nil {
					t.Fatal(err)
				}
			},
			run: func(r *REPL) string { return r.handleMCPStart("secure") },
		},
		{
			name: "test probe",
			setup: func(t *testing.T) {
				withTempMCPLoginHome(t)
				if err := mcp.Save(&mcp.Registry{Servers: []mcp.ServerEntry{{Name: "secure", Command: "unused"}}}); err != nil {
					t.Fatal(err)
				}
			},
			run: func(r *REPL) string { return r.handleMCPTest("secure") },
		},
		{
			name: "computer use",
			setup: func(t *testing.T) {
				withTempCuEnv(t)
			},
			run: cuEnable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup(t)
			m := newSlashTestModel(t)
			r := m.asREPL()
			finished := 0
			r.BeginMCPLaunch = func(context.Context) *MCPLaunchTicket {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return NewMCPLaunchTicket(ctx, nil, func() { finished++ })
			}
			launchCalls := 0
			stubConfiguredMCPLaunch(t, func(context.Context, *mcp.Registry, string, *tools.Registry, *sandbox.Manager) (*mcptools.Server, error) {
				launchCalls++
				return explicitTicketTestServer("unexpected"), nil
			})

			_ = tt.run(r)
			if launchCalls != 0 {
				t.Fatalf("rejected ticket invoked launcher %d time(s)", launchCalls)
			}
			if finished != 1 {
				t.Fatalf("rejected ticket finish calls = %d, want 1", finished)
			}
		})
	}
}

func TestRejectedMCPLoginTicketSkipsOAuthAndLauncher(t *testing.T) {
	withTempMCPLoginHome(t)
	seedOAuthMCP(t, mcp.ServerEntry{
		Name: "secure", URL: "https://mcp.example.test/api", Auth: "oauth",
	})
	m := newSlashTestModel(t)
	r := m.asREPL()
	finished := 0
	r.BeginMCPLaunch = func(context.Context) *MCPLaunchTicket {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return NewMCPLaunchTicket(ctx, nil, func() { finished++ })
	}
	oauthCalls := 0
	stubMCPTokenEnsure(t, func(context.Context, string, string, bool) (string, error) {
		oauthCalls++
		return "token", nil
	})
	launchCalls := 0
	stubMCPLoginStart(t, func(context.Context, context.Context, string, *tools.Registry) (mcpLoginLaunch, error) {
		launchCalls++
		return mcpLoginLaunch{}, nil
	})

	_ = r.handleMCPLogin("secure")
	if oauthCalls != 0 || launchCalls != 0 {
		t.Fatalf("rejected login ticket invoked oauth=%d launcher=%d", oauthCalls, launchCalls)
	}
	if finished != 1 {
		t.Fatalf("rejected login ticket finish calls = %d, want 1", finished)
	}
}
