package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

const mcpLoginTimeout = 2 * time.Minute

type mcpLoginTarget struct {
	name string
	url  string
}

type mcpLoginResultMsg struct {
	requestID uint64
	name      string
	registry  *tools.Registry
	lease     *mcpLoginLaunchLease
	err       error
}

// mcpLoginLaunch is produced off the Bubble Tea Update goroutine and
// published by handleMCPLoginResult. Keeping the staged tools in the typed
// message means the tea.Cmd never mutates Model-visible registry state.
type mcpLoginLaunch struct {
	server mcpLoginServer
	tools  []tools.Tool
}

type mcpLoginServer interface {
	Close() error
}

// mcpLoginLaunchLease owns a successfully launched server between the
// background tea.Cmd and the Bubble Tea Update goroutine. A normal result
// claims the lease before publishing tools. Cancellation or application exit
// aborts it, closing a server even when the result message is never consumed.
// This closes the otherwise unavoidable "launch succeeded while tea.Quit was
// draining" ownership gap.
type mcpLoginLaunchLease struct {
	mu      sync.Mutex
	launch  mcpLoginLaunch
	aborted bool
	claimed bool
}

func (l *mcpLoginLaunchLease) stage(launch mcpLoginLaunch) bool {
	if l == nil {
		closeMCPLoginLaunch(launch)
		return false
	}
	l.mu.Lock()
	if l.aborted || l.claimed {
		l.mu.Unlock()
		closeMCPLoginLaunch(launch)
		return false
	}
	l.launch = launch
	l.mu.Unlock()
	return true
}

func (l *mcpLoginLaunchLease) claim() (mcpLoginLaunch, bool) {
	if l == nil {
		return mcpLoginLaunch{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.aborted || l.claimed {
		return mcpLoginLaunch{}, false
	}
	l.claimed = true
	launch := l.launch
	l.launch = mcpLoginLaunch{}
	return launch, true
}

func (l *mcpLoginLaunchLease) abort() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.claimed || l.aborted {
		l.mu.Unlock()
		return
	}
	l.aborted = true
	launch := l.launch
	l.launch = mcpLoginLaunch{}
	l.mu.Unlock()
	closeMCPLoginLaunch(launch)
}

func closeMCPLoginLaunch(launch mcpLoginLaunch) {
	if launch.server != nil {
		_ = launch.server.Close()
	}
}

// launchMCPServerWithLifecycle gives synchronous `/mcp start` and `/cu enable`
// the same two-phase context handoff as explicit OAuth reconnects: the bounded
// operation context controls only the handshake, while a successful server is
// retained until the REPL/TUI lifecycle ends. Without this handoff, the
// command's deferred timeout cancel immediately kills a freshly started stdio
// child and leaves registered tools pointing at a dead client.
func launchMCPServerWithLifecycle(
	operationCtx, lifecycleCtx context.Context,
	launch func(context.Context) (*mcptools.Server, error),
) (*mcptools.Server, error) {
	if launch == nil {
		return nil, errors.New("MCP launcher is unavailable")
	}
	handoff := newMCPLoginLaunchContext(operationCtx, lifecycleCtx)
	server, err := launch(handoff)
	if err != nil {
		if server != nil {
			_ = server.Close()
		}
		return nil, err
	}
	if server == nil {
		return nil, errors.New("MCP launcher returned no server")
	}
	if !handoff.commit() {
		if server != nil {
			_ = server.Close()
		}
		if operationCtx != nil && operationCtx.Err() != nil {
			return nil, operationCtx.Err()
		}
		if lifecycleCtx != nil && lifecycleCtx.Err() != nil {
			return nil, lifecycleCtx.Err()
		}
		return nil, context.Canceled
	}
	return server, nil
}

// mcpLoginLaunchContext initially follows both the bounded login operation and
// the application lifecycle. Once the handshake succeeds, commit detaches the
// operation deadline while preserving lifecycle cancellation for the live MCP
// client. Passing this context into LaunchServer means Esc genuinely interrupts
// the handshake without making a successful server die when the two-minute
// login context is released.
type mcpLoginLaunchContext struct {
	operation context.Context
	lifecycle context.Context
	done      chan struct{}

	mu        sync.Mutex
	committed bool
	err       error
}

func newMCPLoginLaunchContext(operation, lifecycle context.Context) *mcpLoginLaunchContext {
	if operation == nil {
		operation = context.Background()
	}
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	c := &mcpLoginLaunchContext{
		operation: operation,
		lifecycle: lifecycle,
		done:      make(chan struct{}),
	}
	go c.watch()
	return c
}

func (c *mcpLoginLaunchContext) watch() {
	select {
	case <-c.operation.Done():
		c.finishOperation(c.operation.Err())
	case <-c.lifecycle.Done():
		c.finish(c.lifecycle.Err())
	case <-c.done:
		return
	}
}

func (c *mcpLoginLaunchContext) finishOperation(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.committed || c.err != nil {
		return
	}
	c.err = err
	close(c.done)
}

func (c *mcpLoginLaunchContext) finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	close(c.done)
}

func (c *mcpLoginLaunchContext) commit() bool {
	c.mu.Lock()
	if c.err != nil || c.operation.Err() != nil || c.lifecycle.Err() != nil {
		c.mu.Unlock()
		return false
	}
	c.committed = true
	c.mu.Unlock()

	// The first watcher may now exit without closing done. A dedicated
	// lifecycle watcher keeps the handed-off client tied to application exit.
	go func() {
		select {
		case <-c.lifecycle.Done():
			c.finish(c.lifecycle.Err())
		case <-c.done:
		}
	}()
	return true
}

func (c *mcpLoginLaunchContext) Deadline() (time.Time, bool) {
	c.mu.Lock()
	committed := c.committed
	c.mu.Unlock()
	if committed {
		return c.lifecycle.Deadline()
	}
	opDeadline, opOK := c.operation.Deadline()
	lifeDeadline, lifeOK := c.lifecycle.Deadline()
	if !opOK {
		return lifeDeadline, lifeOK
	}
	if !lifeOK || opDeadline.Before(lifeDeadline) {
		return opDeadline, true
	}
	return lifeDeadline, true
}

func (c *mcpLoginLaunchContext) Done() <-chan struct{} { return c.done }

func (c *mcpLoginLaunchContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *mcpLoginLaunchContext) Value(key any) any {
	if value := c.operation.Value(key); value != nil {
		return value
	}
	return c.lifecycle.Value(key)
}

// launchMCPServerAfterLogin is narrower than mcp.LaunchServer so tests can
// assert which context reaches the actual handshake without opening a browser
// or starting a real MCP process.
var launchMCPServerAfterLogin = func(ctx context.Context, reg *mcp.Registry, name string, registry *tools.Registry) (mcpLoginServer, error) {
	return mcp.LaunchServer(ctx, reg, name, registry)
}

// startMCPServerAfterLogin is a test seam around the slow live-server launch.
// It stages discovered tools in a private registry; handleMCPLoginResult owns
// the only TUI-side publication into the live registry.
var startMCPServerAfterLogin = func(operationCtx, lifecycleCtx context.Context, name string, registry *tools.Registry) (mcpLoginLaunch, error) {
	if registry == nil {
		return mcpLoginLaunch{}, nil
	}
	reg, err := mcp.Load()
	if err != nil {
		return mcpLoginLaunch{}, err
	}
	// The registry can be edited while the user completes the browser flow.
	// Revalidate the selected entry immediately before launch so an explicit
	// HTTP OAuth login can never turn into an unauthenticated HTTP connection or
	// a local stdio process because mcp.toml changed in the meantime.
	entry := mcp.FindServer(reg, name)
	if entry == nil {
		return mcpLoginLaunch{}, fmt.Errorf("mcp login %s: server was removed before reconnect", name)
	}
	expanded, err := mcp.ExpandServerEntry(*entry)
	if err != nil {
		return mcpLoginLaunch{}, err
	}
	if expanded.URL == "" || !strings.EqualFold(strings.TrimSpace(expanded.Auth), "oauth") {
		return mcpLoginLaunch{}, fmt.Errorf("mcp login %s: server is no longer an OAuth HTTP server", name)
	}
	if lifecycleCtx == nil {
		lifecycleCtx = context.Background()
	}

	// Stage registration privately so cancellation cannot publish half-started
	// tools. The handoff context follows the operation during the handshake and
	// the application lifecycle after success.
	type launchResult struct {
		server mcpLoginServer
		staged *tools.Registry
		err    error
	}
	resultCh := make(chan launchResult, 1)
	launchCtx := newMCPLoginLaunchContext(operationCtx, lifecycleCtx)
	go func() {
		staged := tools.NewRegistry()
		srv, launchErr := launchMCPServerAfterLogin(launchCtx, reg, name, staged)
		resultCh <- launchResult{server: srv, staged: staged, err: launchErr}
	}()

	var result launchResult
	select {
	case <-operationCtx.Done():
		go func() {
			late := <-resultCh
			if late.server != nil {
				_ = late.server.Close()
			}
		}()
		return mcpLoginLaunch{}, operationCtx.Err()
	case <-lifecycleCtx.Done():
		go func() {
			late := <-resultCh
			if late.server != nil {
				_ = late.server.Close()
			}
		}()
		return mcpLoginLaunch{}, lifecycleCtx.Err()
	case result = <-resultCh:
	}
	if result.err != nil {
		if result.server != nil {
			_ = result.server.Close()
		}
		return mcpLoginLaunch{}, result.err
	}
	if result.server == nil {
		return mcpLoginLaunch{}, errors.New("MCP launcher returned no server")
	}
	if !launchCtx.commit() {
		if result.server != nil {
			_ = result.server.Close()
		}
		if err := operationCtx.Err(); err != nil {
			return mcpLoginLaunch{}, err
		}
		if err := lifecycleCtx.Err(); err != nil {
			return mcpLoginLaunch{}, err
		}
		return mcpLoginLaunch{}, context.Canceled
	}
	return mcpLoginLaunch{server: result.server, tools: result.staged.All()}, nil
}

func liveMCPToolCount(registry *tools.Registry, serverName string) int {
	if registry == nil {
		return 0
	}
	prefix := "mcp__" + serverName + "__"
	count := 0
	for _, t := range registry.All() {
		if strings.HasPrefix(t.Name(), prefix) {
			count++
		}
	}
	return count
}

func runMCPLogin(ctx, lifecycleCtx context.Context, target mcpLoginTarget, registry *tools.Registry) (mcpLoginLaunch, error) {
	if ctx == nil {
		return mcpLoginLaunch{}, errors.New("OAuth context is unavailable")
	}
	if _, err := ensureMCPToken(ctx, target.name, target.url, true); err != nil {
		return mcpLoginLaunch{}, err
	}
	if err := ctx.Err(); err != nil {
		return mcpLoginLaunch{}, err
	}
	launch, err := startMCPServerAfterLogin(ctx, lifecycleCtx, target.name, registry)
	if err != nil {
		return mcpLoginLaunch{}, fmt.Errorf("authenticated, but live MCP start failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		closeMCPLoginLaunch(launch)
		return mcpLoginLaunch{}, err
	}
	return launch, nil
}

func publishMCPLoginLaunch(registry *tools.Registry, serverName string, launch mcpLoginLaunch) (int, bool) {
	if registry == nil {
		if launch.server != nil {
			_ = launch.server.Close()
		}
		return 0, false
	}
	if launch.server == nil {
		return liveMCPToolCount(registry, serverName), false
	}
	registry.ReplacePrefix("mcp__"+serverName+"__", launch.tools)
	return liveMCPToolCount(registry, serverName), launch.server != nil
}

// publishMCPLoginLaunchContext makes cancellation and publication a single
// ordered boundary for synchronous REPL callers. Cancellation that wins before
// this function closes the staged server and cannot expose its tools; once the
// preflight succeeds, publication owns the server and later lifecycle cleanup
// is responsible for closing it.
func publishMCPLoginLaunchContext(ctx context.Context, registry *tools.Registry, serverName string, launch mcpLoginLaunch) (int, bool, error) {
	if ctx == nil {
		closeMCPLoginLaunch(launch)
		return liveMCPToolCount(registry, serverName), false, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		closeMCPLoginLaunch(launch)
		return liveMCPToolCount(registry, serverName), false, err
	}
	toolCount, ownsServer := publishMCPLoginLaunch(registry, serverName, launch)
	return toolCount, ownsServer, nil
}

func adoptOrPublishMCPLoginLaunch(
	registry *tools.Registry,
	serverName string,
	launch mcpLoginLaunch,
	adopt func(*mcptools.Server, []tools.Tool) bool,
) (int, bool) {
	if concrete, ok := launch.server.(*mcptools.Server); ok && concrete != nil && adopt != nil {
		if adopt(concrete, launch.tools) {
			return liveMCPToolCount(registry, serverName), false
		}
	}
	return publishMCPLoginLaunch(registry, serverName, launch)
}

func redactMCPLoginError(err error) string {
	if err == nil {
		return ""
	}
	return security.RedactSubprocessText(err.Error())
}

func closeMCPLoginServers(servers []mcpLoginServer) error {
	var joined error
	for _, server := range servers {
		if server != nil {
			joined = errors.Join(joined, server.Close())
		}
	}
	return joined
}

func (m *Model) closeMCPLoginServers() error {
	if m == nil {
		return nil
	}
	servers := m.mcpLoginServers
	m.mcpLoginServers = nil
	return closeMCPLoginServers(servers)
}

func (r *REPL) closeMCPLoginServers() error {
	if r == nil {
		return nil
	}
	servers := r.mcpLoginServers
	r.mcpLoginServers = nil
	return closeMCPLoginServers(servers)
}

func (m *Model) startMCPLogin(name string) tea.Cmd {
	if m.mcpLoginPending {
		m.messages = append(m.messages, Message{
			Role: "info", Content: "(MCP OAuth login already in progress)", Timestamp: time.Now(),
		})
		return nil
	}
	target, err := resolveMCPLoginTarget(name)
	if err != nil {
		m.messages = append(m.messages, Message{Role: "error", Content: redactMCPLoginError(err), Timestamp: time.Now()})
		return nil
	}

	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, mcpLoginTimeout)
	lease := &mcpLoginLaunchLease{}
	m.mcpLoginSeq++
	requestID := m.mcpLoginSeq
	m.mcpLoginPending = true
	m.mcpLoginCancel = func() {
		cancel()
		lease.abort()
	}
	m.messages = append(m.messages, Message{
		Role: "info",
		Content: fmt.Sprintf(
			"(MCP server %q OAuth login in progress… complete it in the browser · Esc/Ctrl-C cancels)",
			name,
		),
		Timestamp: time.Now(),
	})

	var registry *tools.Registry
	if m.loop != nil {
		registry = m.loop.Registry
	}
	return func() tea.Msg {
		launch, err := runMCPLogin(ctx, base, target, registry)
		if err == nil && !lease.stage(launch) {
			err = context.Canceled
		}
		return mcpLoginResultMsg{
			requestID: requestID, name: name, registry: registry,
			lease: lease, err: err,
		}
	}
}

func (m *Model) cancelMCPLogin() bool {
	if m == nil || !m.mcpLoginPending || m.mcpLoginCancel == nil {
		return false
	}
	m.mcpLoginCancel()
	m.mcpLoginCancel = nil
	return true
}

func (m *Model) handleMCPLoginResult(msg mcpLoginResultMsg) {
	if !m.mcpLoginPending || msg.requestID != m.mcpLoginSeq {
		msg.lease.abort()
		return
	}
	var launch mcpLoginLaunch
	if msg.err == nil {
		var claimed bool
		launch, claimed = msg.lease.claim()
		if !claimed {
			msg.err = context.Canceled
		}
	} else {
		msg.lease.abort()
	}
	if m.mcpLoginCancel != nil {
		m.mcpLoginCancel()
	}
	m.mcpLoginCancel = nil
	m.mcpLoginPending = false
	if msg.err == nil && m.ctx != nil && m.ctx.Err() != nil {
		msg.err = m.ctx.Err()
	}
	if msg.err != nil {
		closeMCPLoginLaunch(launch)
		role := "error"
		content := fmt.Sprintf("mcp login %s: %s", msg.name, redactMCPLoginError(msg.err))
		if errors.Is(msg.err, context.Canceled) || errors.Is(msg.err, context.DeadlineExceeded) {
			role = "warning"
			content = fmt.Sprintf("(MCP server %q OAuth login canceled or timed out)", msg.name)
		}
		m.messages = append(m.messages, Message{Role: role, Content: content, Timestamp: time.Now()})
		return
	}
	toolCount, ownsServer := adoptOrPublishMCPLoginLaunch(msg.registry, msg.name, launch, m.ext.AdoptMCPServer)
	if ownsServer {
		m.mcpLoginServers = append(m.mcpLoginServers, launch.server)
	}
	m.messages = append(m.messages, Message{
		Role: "success",
		Content: fmt.Sprintf(
			"(MCP server %q OAuth login complete · %d tools available in this session)",
			msg.name, toolCount,
		),
		Timestamp: time.Now(),
	})
}
