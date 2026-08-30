package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/processutil"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

// --- helpers ---------------------------------------------------------------

// newClientWithStdio wires a Client around a stdio-shaped transport using
// pipes so we can drive the read loop from the test.
func newClientWithStdio(t *testing.T) (*Client, *io.PipeWriter, *failingWriter) {
	t.Helper()
	stdoutR, stdoutW := io.Pipe() // server → client (we write to stdoutW)
	failW := &failingWriter{}     // client → server (test inspects writes)

	tr := &StdioTransport{stdin: failW, stdout: stdoutR}
	c := NewClient(context.Background(), tr)
	go c.readLoop()
	return c, stdoutW, failW
}

// failingWriter is an io.WriteCloser that records writes and can be made
// to return errors on demand (simulates a dead subprocess pipe).
type failingWriter struct {
	mu      sync.Mutex
	written []byte
	failNow bool
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failNow {
		return 0, errors.New("pipe closed")
	}
	w.written = append(w.written, p...)
	return len(p), nil
}
func (w *failingWriter) Close() error { return nil }

// --- tests -----------------------------------------------------------------

func TestSend_GeneratesUniqueIDsUnderLoad(t *testing.T) {
	c := NewClient(context.Background(), nil)
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("%d", c.idSeq.Add(1))
		if seen[id] {
			t.Fatalf("duplicate id at iter %d: %s", i, id)
		}
		seen[id] = true
	}
}

func TestSend_StdioWriteErrorSurfacesImmediately(t *testing.T) {
	c, _, failW := newClientWithStdio(t)
	failW.mu.Lock()
	failW.failNow = true
	failW.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := c.send(ctx, "test/method", nil)
	if err == nil {
		t.Fatal("expected immediate error from failed write")
	}
	if ctx.Err() != nil {
		t.Errorf("ctx timed out instead of failing fast: %v", ctx.Err())
	}
}

func TestSend_PendingEntryDeletedOnSuccess(t *testing.T) {
	c, stdoutW, _ := newClientWithStdio(t)

	respDone := make(chan error, 1)
	go func() {
		_, err := c.send(context.Background(), "ping", nil)
		respDone <- err
	}()

	// Read the line the client wrote, parse the id, send a fake response back.
	var req JSONRPCRequest
	for {
		c.mu.RLock()
		n := len(c.pending)
		c.mu.RUnlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	c.mu.RLock()
	for id := range c.pending {
		req.ID = id
	}
	c.mu.RUnlock()

	resp := JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"ok":true}`)}
	b, _ := json.Marshal(resp)
	stdoutW.Write(append(b, '\n'))

	if err := <-respDone; err != nil {
		t.Fatalf("send returned err: %v", err)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.pending) != 0 {
		t.Errorf("pending should be empty after success, got %d", len(c.pending))
	}
}

func TestSend_PendingEntryDeletedOnCancel(t *testing.T) {
	c, _, _ := newClientWithStdio(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.send(ctx, "ping", nil)
		done <- err
	}()

	// Wait for send to register the pending entry, then cancel.
	for {
		c.mu.RLock()
		n := len(c.pending)
		c.mu.RUnlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	if err := <-done; err == nil {
		t.Fatal("expected error after cancel")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.pending) != 0 {
		t.Errorf("pending should be cleaned up after cancel, got %d entries", len(c.pending))
	}
}

func TestReadLoop_CancellingClientCtxWakesPendingSenders(t *testing.T) {
	c, stdoutW, _ := newClientWithStdio(t)

	done := make(chan error, 1)
	go func() {
		_, err := c.send(context.Background(), "ping", nil)
		done <- err
	}()

	// Wait for pending registration.
	for {
		c.mu.RLock()
		n := len(c.pending)
		c.mu.RUnlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Simulate the subprocess dying: close server-side stdout. readLoop hits
	// EOF, calls c.cancel(), the sender wakes with ErrTransportClosed.
	stdoutW.Close()

	select {
	case err := <-done:
		if !errors.Is(err, ErrTransportClosed) {
			t.Errorf("expected ErrTransportClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not return after readLoop exited")
	}
}

func TestSanitizedStdioEnv_DropsAmbientSecretsAndKeepsExplicitServerEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("METIS_TEST_AMBIENT_SECRET", "must-not-reach-mcp")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-provider-key-must-not-reach-mcp")
	for key, value := range map[string]string{
		"DISPLAY": ":42", "WAYLAND_DISPLAY": "wayland-42",
		"XAUTHORITY": "/tmp/ambient-xauthority", "XDG_RUNTIME_DIR": "/tmp/ambient-runtime",
		"DBUS_SESSION_BUS_ADDRESS": "unix:path=/tmp/ambient-bus",
	} {
		t.Setenv(key, value)
	}

	env := sanitizedStdioEnv([]string{
		"MCP_SERVER_TOKEN=explicit-server-token",
		"PATH=/opt/mcp/bin:/usr/bin:/bin",
		"AGENT=other-agent",
		"AI_AGENT=other-agent",
		"METIS=0",
		"DISPLAY=:explicit",
		"WAYLAND_DISPLAY=wayland-explicit",
		"XAUTHORITY=/tmp/explicit-xauthority",
		"XDG_RUNTIME_DIR=/tmp/explicit-runtime",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/explicit-bus",
		"METIS_INTERNAL_SANDBOX_PROFILE=stdio-mcp-desktop",
	}, "/tmp/mcp-working-dir")
	got := envMap(env)

	if _, ok := got["METIS_TEST_AMBIENT_SECRET"]; ok {
		t.Fatal("ambient secret leaked into stdio MCP environment")
	}
	if _, ok := got["ANTHROPIC_API_KEY"]; ok {
		t.Fatal("provider credential leaked into stdio MCP environment")
	}
	for _, key := range []string{
		"DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "XDG_RUNTIME_DIR",
		"DBUS_SESSION_BUS_ADDRESS", "METIS_INTERNAL_SANDBOX_PROFILE",
	} {
		if _, ok := got[key]; ok {
			t.Fatalf("generic stdio environment exposed reserved capability %s: %#v", key, got)
		}
	}
	if got["MCP_SERVER_TOKEN"] != "explicit-server-token" {
		t.Fatalf("explicit MCP server environment missing: %#v", got)
	}
	if got["PATH"] != "/opt/mcp/bin:/usr/bin:/bin" {
		t.Fatalf("explicit PATH override = %q, want MCP-specific value", got["PATH"])
	}
	if got["HOME"] != os.Getenv("HOME") || got["LANG"] != "en_US.UTF-8" {
		t.Fatalf("minimal launch environment lost essentials: %#v", got)
	}
	if got["PWD"] != "/tmp/mcp-working-dir" {
		t.Fatalf("PWD = %q, want working directory", got["PWD"])
	}
	for key, want := range map[string]string{
		"AGENT": "metis", "AI_AGENT": "metis", "METIS": "1",
	} {
		if got[key] != want {
			t.Fatalf("noninteractive marker %s = %q, want %q", key, got[key], want)
		}
	}
}

func TestSanitizedStdioEnv_ComputerUseAllowsOnlyX11Handles(t *testing.T) {
	env := sanitizedStdioEnvForProfile([]string{
		"DISPLAY=:88",
		"XAUTHORITY=/tmp/cu-xauthority",
		"WAYLAND_DISPLAY=wayland-88",
		"XDG_RUNTIME_DIR=/tmp/cu-runtime",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/cu-bus",
		"METIS_INTERNAL_SANDBOX_PROFILE=stdio-mcp",
	}, "/tmp/cu-working-dir", StdioSandboxProfileComputerUse)
	got := envMap(env)
	if got["DISPLAY"] != ":88" || got["XAUTHORITY"] != "/tmp/cu-xauthority" {
		t.Fatalf("Computer Use lost required X11 handles: %#v", got)
	}
	for _, key := range []string{
		"WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
		"METIS_INTERNAL_SANDBOX_PROFILE",
	} {
		if _, ok := got[key]; ok {
			t.Fatalf("Computer Use received unnecessary/forgeable capability %s: %#v", key, got)
		}
	}
}

func TestNewStdioTransportWithEnv_IsolatesActualChildEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh to inspect the child environment")
	}
	t.Setenv("METIS_TEST_AMBIENT_SECRET", "ambient-secret-must-not-leak")
	const explicitSecret = "opaque-explicit-mcp-secret"

	tpt, err := NewStdioTransportWithEnv(
		context.Background(),
		"/bin/sh",
		[]string{"MCP_SERVER_TOKEN=" + explicitSecret},
		"-c",
		`printf 'ambient=%s explicit=%s\n' "${METIS_TEST_AMBIENT_SECRET-missing}" "$MCP_SERVER_TOKEN" >&2`,
	)
	if err != nil {
		t.Fatalf("start child: %v", err)
	}
	waitForStderrContains(t, tpt, "ambient=missing")
	if err := tpt.Close(); err != nil {
		t.Fatalf("close child: %v", err)
	}
	if err := tpt.Close(); err != nil {
		t.Fatalf("second close child: %v", err)
	}
	got := tpt.Stderr()
	if !strings.Contains(got, "ambient=missing") {
		t.Fatalf("ambient variable unexpectedly reached child: %q", got)
	}
	if strings.Contains(got, "ambient-secret-must-not-leak") || strings.Contains(got, explicitSecret) {
		t.Fatalf("child stderr leaked a credential: %q", got)
	}
	if !strings.Contains(got, "explicit=[REDACTED]") {
		t.Fatalf("explicit server credential was not redacted: %q", got)
	}
}

func TestNewStdioTransportWithSandboxFailsClosedBeforeStartingChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh to observe whether the child started")
	}
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "started")
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	transport, err := NewStdioTransportWithEnvAndDirAndSandbox(
		context.Background(), "/bin/sh", nil, "", manager,
		"-c", "printf started > \"$1\"", "sh", marker,
	)
	if transport != nil || !errors.Is(err, sandbox.ErrManagerClosed) {
		t.Fatalf("closed-manager launch = (%#v, %v), want nil ErrManagerClosed", transport, err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("child ran before sandbox failure: stat error = %v", statErr)
	}
}

func TestNewStdioTransportWithSandboxNormalizesTempAndKeepsExplicitEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh to inspect the child environment")
	}
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{
		Mode:     string(sandbox.ModeOff),
		TempRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	t.Setenv("TMPDIR", t.TempDir())
	const explicit = "explicit-mcp-value"

	transport, err := NewStdioTransportWithEnvAndDirAndSandbox(
		context.Background(), "/bin/sh", []string{
			"MCP_EXPLICIT_VALUE=" + explicit,
			"EXPECTED_TMPDIR=" + manager.TempDir(),
		}, "", manager,
		"-c", `test "$MCP_EXPLICIT_VALUE" = "`+explicit+`" && test "$TMPDIR" = "$EXPECTED_TMPDIR" && printf sandbox-env-ok >&2`,
	)
	if err != nil {
		t.Fatalf("start sandboxed child: %v", err)
	}
	if sandbox.Available() && transport.cmd.Path == "/bin/sh" {
		_ = transport.Close()
		t.Fatal("stdio MCP stayed unsandboxed even though credential isolation is available")
	}
	waitForStderrContains(t, transport, "sandbox-env-ok")
	if err := transport.Close(); err != nil {
		t.Fatalf("close sandboxed child: %v", err)
	}
	if got := transport.Stderr(); !strings.Contains(got, "sandbox-env-ok") {
		t.Fatalf("sandboxed child environment check failed: %q", got)
	}
}

func TestLinuxStdioMCPHidesMetisCredentialsCreatedAfterLaunch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux bubblewrap mount namespaces")
	}
	diagnostic := sandbox.Doctor()
	if !diagnostic.Available {
		t.Skipf("bubblewrap unavailable: %v", diagnostic.Err)
	}
	probe := exec.Command(diagnostic.Executable,
		"--die-with-parent", "--ro-bind", "/", "/", "--", "/bin/true")
	if err := probe.Run(); err != nil {
		t.Skipf("bubblewrap cannot create a sandbox here: %v", err)
	}

	// Installed MCP bundles live below METIS_HOME, so the credential boundary
	// must not make their declared working directory or relative assets vanish.
	home := t.TempDir()
	t.Setenv("HOME", home)
	defaultMetisHome := filepath.Join(home, ".metis")
	metisHome := filepath.Join(t.TempDir(), "custom-metis-home")
	pluginDir := filepath.Join(metisHome, "plugins", "fixture")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "asset.txt"), []byte("plugin-ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "auth.json"), []byte("plugin-local"), 0o600); err != nil {
		t.Fatal(err)
	}
	serverPath := filepath.Join(pluginDir, "bin", "server")
	if err := os.MkdirAll(filepath.Dir(serverPath), 0o700); err != nil {
		t.Fatal(err)
	}
	serverScript := `#!/bin/sh
test "$(cat asset.txt)" = plugin-ok || { printf 'asset-before=missing\n' >&2; exit 31; }
test "$(cat auth.json)" = plugin-local || { printf 'plugin-auth=missing\n' >&2; exit 32; }
if test -n "${METIS_INTERNAL_SANDBOX_PROFILE+x}"; then
	printf 'internal-profile=visible\n' >&2
else
	printf 'internal-profile=hidden\n' >&2
fi
printf 'ready\n' >&2
IFS= read -r _
for name in auth.json mcp-oauth.json; do
	if test -r "$1/$name"; then
		printf 'active-%s=visible\n' "$name" >&2
	else
		printf 'active-%s=hidden\n' "$name" >&2
	fi
	if test -r "$2/$name"; then
		printf 'default-%s=visible\n' "$name" >&2
	else
		printf 'default-%s=hidden\n' "$name" >&2
	fi
done
test "$(cat asset.txt)" = plugin-ok && printf 'asset-after=visible\n' >&2
test "$(cat auth.json)" = plugin-local && printf 'plugin-auth=visible\n' >&2
`
	if err := os.WriteFile(serverPath, []byte(serverScript), 0o700); err != nil {
		t.Fatal(err)
	}
	pluginAlias := filepath.Join(t.TempDir(), "plugin-alias")
	if err := os.Symlink(pluginDir, pluginAlias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{
		Mode:      string(sandbox.ModeOff),
		TempRoot:  t.TempDir(),
		MetisHome: metisHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	transport, err := NewStdioTransportWithEnvAndDirAndSandbox(
		context.Background(), serverPath, nil, pluginAlias, manager, metisHome, defaultMetisHome,
	)
	if err != nil {
		t.Fatalf("start sandboxed stdio MCP: %v", err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	waitForStderrContains(t, transport, "ready")

	// OAuth can create either store after this long-lived process has entered
	// its mount namespace. A read-only bind of / is not a snapshot: without a
	// directory-level Metis view, the new directory entries become readable.
	for _, root := range []string{metisHome, defaultMetisHome} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			_ = transport.Close()
			t.Fatal(err)
		}
		for _, name := range []string{"auth.json", "mcp-oauth.json"} {
			if err := os.WriteFile(filepath.Join(root, name), []byte("post-launch-token"), 0o600); err != nil {
				_ = transport.Close()
				t.Fatal(err)
			}
		}
	}
	if _, err := io.WriteString(transport.stdin, "inspect\n"); err != nil {
		_ = transport.Close()
		t.Fatalf("release stdio MCP probe: %v", err)
	}
	waitForStderrContains(t, transport, "asset-after=visible")
	if err := transport.Close(); err != nil {
		t.Fatalf("close sandboxed stdio MCP: %v", err)
	}

	stderr := transport.Stderr()
	for _, rootLabel := range []string{"active", "default"} {
		for _, name := range []string{"auth.json", "mcp-oauth.json"} {
			label := rootLabel + "-" + name
			if strings.Contains(stderr, label+"=visible") {
				t.Errorf("long-lived stdio MCP could read %s created after launch; stderr=%q", label, stderr)
			}
			if !strings.Contains(stderr, label+"=hidden") {
				t.Errorf("stdio MCP did not report %s hidden; stderr=%q", label, stderr)
			}
		}
	}
	if !strings.Contains(stderr, "asset-after=visible") {
		t.Errorf("Metis plugin working directory became inaccessible; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "plugin-auth=visible") {
		t.Errorf("ordinary same-named file inside plugin cwd became inaccessible; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "internal-profile=hidden") || strings.Contains(stderr, "internal-profile=visible") {
		t.Errorf("internal sandbox profile leaked to stdio MCP; stderr=%q", stderr)
	}
}

func TestLinuxStdioMCPRemasksNestedControlRootAfterPluginCwd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux bubblewrap mount namespaces")
	}
	diagnostic := sandbox.Doctor()
	if !diagnostic.Available {
		t.Skipf("bubblewrap unavailable: %v", diagnostic.Err)
	}
	probe := exec.Command(diagnostic.Executable,
		"--die-with-parent", "--ro-bind", "/", "/", "--", "/bin/true")
	if err := probe.Run(); err != nil {
		t.Skipf("bubblewrap cannot create a sandbox here: %v", err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	defaultMetisHome := filepath.Join(home, ".metis")
	pluginDir := filepath.Join(defaultMetisHome, "plugins", "fixture")
	customMetisHome := filepath.Join(pluginDir, "state")
	if err := os.MkdirAll(filepath.Join(pluginDir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "asset.txt"), []byte("plugin-ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	serverPath := filepath.Join(pluginDir, "bin", "server")
	serverScript := `#!/bin/sh
test "$(cat asset.txt)" = plugin-ok || exit 31
printf 'ready\n' >&2
IFS= read -r _
for name in auth.json mcp-oauth.json; do
	if test -r "$1/$name"; then
		printf 'nested-%s=visible\n' "$name" >&2
	else
		printf 'nested-%s=hidden\n' "$name" >&2
	fi
done
test "$(cat asset.txt)" = plugin-ok && printf 'done\n' >&2
`
	if err := os.WriteFile(serverPath, []byte(serverScript), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{
		Mode:      string(sandbox.ModeOff),
		TempRoot:  t.TempDir(),
		MetisHome: customMetisHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	transport, err := NewStdioTransportWithEnvAndDirAndSandbox(
		context.Background(), serverPath, nil, pluginDir, manager, customMetisHome,
	)
	if err != nil {
		t.Fatalf("start nested-root stdio MCP: %v", err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	waitForStderrContains(t, transport, "ready")
	for _, name := range []string{"auth.json", "mcp-oauth.json"} {
		if err := os.WriteFile(filepath.Join(customMetisHome, name), []byte("post-launch-token"), 0o600); err != nil {
			_ = transport.Close()
			t.Fatal(err)
		}
	}
	if _, err := io.WriteString(transport.stdin, "inspect\n"); err != nil {
		_ = transport.Close()
		t.Fatal(err)
	}
	waitForStderrContains(t, transport, "done")
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	stderr := transport.Stderr()
	for _, name := range []string{"auth.json", "mcp-oauth.json"} {
		if strings.Contains(stderr, "nested-"+name+"=visible") || !strings.Contains(stderr, "nested-"+name+"=hidden") {
			t.Errorf("plugin cwd re-exposed nested control root %s; stderr=%q", name, stderr)
		}
	}
}

func TestLinuxStdioMCPMasksDefaultRootNestedBelowCustomRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux bubblewrap mount namespaces")
	}
	diagnostic := sandbox.Doctor()
	if !diagnostic.Available {
		t.Skipf("bubblewrap unavailable: %v", diagnostic.Err)
	}
	probe := exec.Command(diagnostic.Executable,
		"--die-with-parent", "--ro-bind", "/", "/", "--", "/bin/true")
	if err := probe.Run(); err != nil {
		t.Skipf("bubblewrap cannot create a sandbox here: %v", err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	defaultMetisHome := filepath.Join(home, ".metis")
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{
		Mode:      string(sandbox.ModeOff),
		TempRoot:  t.TempDir(),
		MetisHome: home,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	transport, err := NewStdioTransportWithEnvAndDirAndSandbox(
		context.Background(), "/bin/sh", nil, t.TempDir(), manager,
		"-c", `
			printf 'ready\n' >&2
			IFS= read -r _
			for spec in "active:$1" "default:$2"; do
				label=${spec%%:*}
				root=${spec#*:}
				if test -r "$root/auth.json"; then
					printf '%s-auth=visible\n' "$label" >&2
				else
					printf '%s-auth=hidden\n' "$label" >&2
				fi
			done
			printf 'done\n' >&2
		`, "sh", home, defaultMetisHome,
	)
	if err != nil {
		t.Fatalf("start stdio MCP with nested control roots: %v", err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	waitForStderrContains(t, transport, "ready")
	for _, root := range []string{home, defaultMetisHome} {
		if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte("post-launch-token"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := io.WriteString(transport.stdin, "inspect\n"); err != nil {
		t.Fatal(err)
	}
	waitForStderrContains(t, transport, "done")
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	stderr := transport.Stderr()
	for _, label := range []string{"active-auth", "default-auth"} {
		if strings.Contains(stderr, label+"=visible") || !strings.Contains(stderr, label+"=hidden") {
			t.Errorf("nested control-root topology exposed %s; stderr=%q", label, stderr)
		}
	}
}

func waitForStderrContains(t *testing.T, transport *StdioTransport, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(transport.Stderr(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stdio child did not emit %q; stderr=%q", want, transport.Stderr())
}

func TestStdioTransportHoldsSandboxLeaseUntilClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh as an MCP-shaped stdin child")
	}
	manager, err := sandbox.NewManagerWithOptions(sandbox.Options{
		Mode:     string(sandbox.ModeOff),
		TempRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := NewStdioTransportWithEnvAndDirAndSandbox(
		context.Background(), "/bin/sh", nil, "", manager,
		"-c", "cat >/dev/null",
	)
	if err != nil {
		_ = manager.Close()
		t.Fatal(err)
	}

	managerClosed := make(chan error, 1)
	go func() { managerClosed <- manager.Close() }()
	select {
	case err := <-managerClosed:
		_ = transport.Close()
		t.Fatalf("sandbox manager closed while MCP transport was active: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-managerClosed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sandbox manager did not close after MCP transport released its lease")
	}
}

func TestStdioTransportCloseKillsDescendantThatInheritedStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh and a Unix PID probe")
	}
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	transport, err := NewStdioTransportWithEnv(
		context.Background(), "/bin/sh", nil,
		"-c", `(sleep 30) >&2 & echo $! > "$1"; read _`, "sh", pidFile,
	)
	if err != nil {
		t.Fatal(err)
	}
	var descendantPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			descendantPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && descendantPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if descendantPID <= 0 {
		_ = transport.Close()
		t.Fatal("child did not publish descendant pid")
	}
	descendant, _ := os.FindProcess(descendantPID)
	t.Cleanup(func() {
		if descendant != nil {
			_ = descendant.Kill()
		}
	})

	closed := make(chan error, 1)
	go func() { closed <- transport.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(750 * time.Millisecond):
		_ = descendant.Kill()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("Close blocked on stderr inherited by a descendant process")
	}

	for deadline := time.Now().Add(time.Second); processutil.Alive(descendantPID) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if processutil.Alive(descendantPID) {
		t.Fatalf("descendant process %d survived transport Close", descendantPID)
	}
}

func TestStdioTransportStderr_RedactsConfiguredAndRecognizableSecrets(t *testing.T) {
	const configuredSecret = "opaque-mcp-secret-value"
	const genericPassword = "opaque-generic-password"
	const urlPassword = "opaque-url-password"
	githubToken := "ghp_" + strings.Repeat("a", 36)
	b := newBoundedBuffer(4096)
	_, _ = b.Write([]byte("MCP_SERVER_TOKEN=" + configuredSecret +
		" Authorization: Bearer " + strings.Repeat("B", 32) +
		" github=" + githubToken +
		" CUSTOM_PASSWORD=" + genericPassword +
		" endpoint=https://alice:" + urlPassword + "@example.test/private"))
	tpt := &StdioTransport{
		stderrBuf:    b,
		redactValues: []string{configuredSecret},
	}

	got := tpt.Stderr()
	for _, secret := range []string{configuredSecret, genericPassword, urlPassword, githubToken, strings.Repeat("B", 32)} {
		if strings.Contains(got, secret) {
			t.Fatalf("stderr leaked secret %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("stderr did not mark redaction: %s", got)
	}
}

func TestClientMCPError_RedactsServerEchoedSecret(t *testing.T) {
	const configuredSecret = "opaque-server-error-secret"
	c := NewClient(context.Background(), &StdioTransport{redactValues: []string{configuredSecret}})

	err := c.mcpResponseError(&JSONRPCError{
		Code:    -32000,
		Message: "upstream rejected token " + configuredSecret,
	})
	if strings.Contains(err.Error(), configuredSecret) {
		t.Fatalf("MCP JSON-RPC error leaked configured secret: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("MCP JSON-RPC error did not mark redaction: %v", err)
	}
}

func TestClientMCPError_RedactsOpaqueHTTPHeaderCredentials(t *testing.T) {
	headers := map[string]string{
		"Authorization":   "Opaque opaque-authorization-value",
		"X-API-Key":       "opaque-http-api-key-value",
		"X-Session-Token": "opaque-session-token-value",
		"X-Client-Secret": "opaque-client-secret-value",
		"X-Trace-ID":      "trace-id-must-remain-visible",
	}
	c := NewClient(context.Background(), &HTTPTransport{headers: headers})
	err := c.mcpResponseError(&JSONRPCError{
		Code: -32001,
		Message: strings.Join([]string{
			headers["Authorization"],
			headers["X-API-Key"],
			headers["X-Session-Token"],
			headers["X-Client-Secret"],
			headers["X-Trace-ID"],
		}, " "),
	})

	got := err.Error()
	for _, key := range []string{"Authorization", "X-API-Key", "X-Session-Token", "X-Client-Secret"} {
		if strings.Contains(got, headers[key]) {
			t.Fatalf("MCP JSON-RPC error leaked HTTP credential from %s: %v", key, err)
		}
	}
	if !strings.Contains(got, headers["X-Trace-ID"]) {
		t.Fatalf("non-credential diagnostic header was unexpectedly redacted: %v", err)
	}
}

func TestClientMCPError_RedactsHTTPAuthorizationSchemePayloads(t *testing.T) {
	for _, tc := range []struct {
		scheme  string
		payload string
	}{
		{scheme: "Bearer", payload: "raw-opaque-bearer-value"},
		{scheme: "Basic", payload: "dXNlcjpwYXNz"},
		{scheme: "ApiKey", payload: "raw-opaque-apikey-value"},
		{scheme: "GNAP", payload: "raw-opaque-gnap-value"},
		{scheme: "PrivateScheme", payload: "raw-private-scheme-value"},
	} {
		t.Run(tc.scheme, func(t *testing.T) {
			c := NewClient(context.Background(), &HTTPTransport{headers: map[string]string{
				"Authorization": tc.scheme + " " + tc.payload,
			}})
			err := c.mcpResponseError(&JSONRPCError{
				Code:    -32002,
				Message: "upstream rejected raw credential " + tc.payload,
			})
			if strings.Contains(err.Error(), tc.payload) {
				t.Fatalf("MCP error leaked raw %s payload: %v", tc.scheme, err)
			}
		})
	}
}

func TestClientRedactText_RedactsWebhookAndOpaqueLongURLButKeepsPublicURL(t *testing.T) {
	const webhook = "https://hooks.example.test/services/T00000000/B00000000/opaque-webhook-value"
	const longSecretURL = "https://api.example.test/session/0123456789abcdefABCDEF9876543210/callback"
	const publicURL = "https://docs.example.test/guides/model-context-protocol/resources"
	c := NewClient(context.Background(), &HTTPTransport{endpoint: webhook, headers: map[string]string{
		"X-Runtime-URL": longSecretURL,
		"X-Public-Docs": publicURL,
	}})

	got := c.RedactText(strings.Join([]string{webhook, longSecretURL, publicURL}, "\n"))
	if strings.Contains(got, webhook) || strings.Contains(got, longSecretURL) {
		t.Fatalf("MCP text leaked an explicitly configured secret URL: %q", got)
	}
	if !strings.Contains(got, publicURL) {
		t.Fatalf("ordinary public URL was unexpectedly redacted: %q", got)
	}
	cause := errors.New("connect " + webhook + ": refused")
	redactedErr := c.redactError(cause)
	if strings.Contains(redactedErr.Error(), webhook) {
		t.Fatalf("transport error leaked endpoint: %v", redactedErr)
	}
	if errors.Is(redactedErr, cause) || errors.Unwrap(redactedErr) == cause {
		t.Fatalf("redacted transport error exposed its raw secret-bearing cause: %v", errors.Unwrap(redactedErr))
	}
	classified := c.redactError(fmt.Errorf("request failed: %w", context.DeadlineExceeded))
	if !errors.Is(classified, context.DeadlineExceeded) {
		t.Fatalf("redacted transport error lost safe deadline classification: %v", classified)
	}
}

func TestClientMCPError_RedactsStdioCredentialSchemePayload(t *testing.T) {
	const payload = "raw-opaque-stdio-token"
	c := NewClient(context.Background(), &StdioTransport{
		redactValues: configuredSecretEnvValues([]string{
			"MCP_AUTHORIZATION=Bearer " + payload,
		}),
	})
	err := c.mcpResponseError(&JSONRPCError{
		Code:    -32003,
		Message: "child rejected raw credential " + payload,
	})
	if strings.Contains(err.Error(), payload) {
		t.Fatalf("MCP error leaked raw stdio credential payload: %v", err)
	}
}

func TestClientRedactText_RedactsStdioCredentialSchemePayload(t *testing.T) {
	const payload = "raw-opaque-stdio-output-token"
	c := NewClient(context.Background(), &StdioTransport{
		redactValues: configuredSecretEnvValues([]string{
			"MCP_AUTHORIZATION=Bearer " + payload,
		}),
	})

	got := c.RedactText("successful tool output echoed " + payload)
	if strings.Contains(got, payload) {
		t.Fatalf("successful MCP text leaked raw stdio credential payload: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("successful MCP text did not mark stdio credential redaction: %q", got)
	}
}

func TestClientRedactText_RedactsHTTPAuthorizationSchemePayload(t *testing.T) {
	const payload = "raw-opaque-http-output-token"
	c := NewClient(context.Background(), &HTTPTransport{headers: map[string]string{
		"Authorization": "Bearer " + payload,
	}})

	got := c.RedactText("successful tool output echoed " + payload)
	if strings.Contains(got, payload) {
		t.Fatalf("successful MCP text leaked raw HTTP credential payload: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("successful MCP text did not mark HTTP credential redaction: %q", got)
	}
}

func envMap(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}
