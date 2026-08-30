// Package mcp implements a client for the Model Context Protocol (MCP).
// Supports both stdio (local subprocess) and HTTP+SSE (remote server) transports.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/security"
)

// ErrTransportClosed is returned to pending senders when the read loop
// exits unexpectedly (subprocess crash, EOF, etc).
var ErrTransportClosed = errors.New("mcp: transport closed")

var (
	errHTTPCrossOriginRedirect = errors.New("mcp: cross-origin HTTP redirect rejected")
	errHTTPTooManyRedirects    = errors.New("mcp: stopped after 10 HTTP redirects")
)

type redactedTransportError struct {
	message        string
	classification error
}

func (e *redactedTransportError) Error() string { return e.message }
func (e *redactedTransportError) Unwrap() error { return e.classification }

// safeErrorClassification retains only non-secret sentinel classifications.
// The original transport error may embed a credential-bearing URL in its
// concrete value, so exposing it through Unwrap would undo Error redaction.
func safeErrorClassification(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, ErrTransportClosed):
		return ErrTransportClosed
	default:
		return nil
	}
}

// Three-layer timeout model — same shape as Claude Code's
// services/mcp/client.ts uses internally:
//
//   - Connect (default 30 s, env MCP_CONNECT_TIMEOUT) — handshake +
//     initial tools/list. Without this a hung server keeps the user
//     staring at "starting…" until they Ctrl+C.
//   - Request (default 60 s, env MCP_REQUEST_TIMEOUT) — bookkeeping
//     RPCs after handshake (re-listing tools / etc).
//   - Tool (default 27.8 h ~ effectively infinite, env MCP_TOOL_TIMEOUT)
//     — `tools/call`. Long because some tools (browser sessions,
//     long-running data jobs) legitimately run for hours; Claude Code
//     uses 100,000,000 ms for the same reason.
//
// All env vars accept Go duration syntax (e.g. "45s", "5m", "1h"); a
// bad/empty value falls back to the default rather than panicking.
const (
	// 30s budget. Tried 10s on 2026-05-18 morning to fail-fast on
	// slow `npx` cold starts; reverted same day because real-world
	// stdio servers (firecrawl-mcp + playwright-mcp under user's
	// config) consistently took >10s for npm boot + browser-binary
	// check even on warm cache. The 10s value made every prompt
	// re-spawn → re-timeout, spamming "MCP handshake: context
	// deadline exceeded" into the chat after each user turn (image
	// #7 / user report 2026-05-18 noon). 30s is the safe default;
	// raise via MCP_CONNECT_TIMEOUT=Xs for truly cold caches.
	defaultConnectTimeout = 30 * time.Second
	defaultRequestTimeout = 60 * time.Second
	defaultToolTimeout    = 100_000 * time.Second // ~27.8 h
)

// ConnectTimeout returns the per-server connect+handshake budget.
func ConnectTimeout() time.Duration {
	return durationFromEnv("MCP_CONNECT_TIMEOUT", defaultConnectTimeout)
}

// RequestTimeout returns the per-RPC budget for non-tool calls
// (tools/list, ping, etc).
func RequestTimeout() time.Duration {
	return durationFromEnv("MCP_REQUEST_TIMEOUT", defaultRequestTimeout)
}

// ToolTimeout returns the per-tools/call budget.
func ToolTimeout() time.Duration {
	return durationFromEnv("MCP_TOOL_TIMEOUT", defaultToolTimeout)
}

func durationFromEnv(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// Transport is the underlying communication mechanism.
type Transport interface {
	Close() error
}

// MaxStderrBytes caps how much stderr we keep accumulating from an MCP
// subprocess. Without a cap, a server stuck in a logspam loop (or a
// poorly-configured tool dumping a download progress bar) would grow
// the buffer unbounded — Claude Code hit this in production and
// settled on 64 MiB as the trade-off (client.ts:982). Anything past
// the cap is silently discarded; the transport keeps draining the
// pipe so the kernel's pipe buffer doesn't deadlock the subprocess.
const MaxStderrBytes = 64 * 1024 * 1024

// maxMCPMessageBytes applies before JSON decoding or recursive redaction.
// MCP messages are expected to be compact JSON; a 16 MiB ceiling leaves room
// for large schemas and text resources while preventing an untrusted server
// from forcing an unbounded allocation on stdio, HTTP, or SSE transports.
const maxMCPMessageBytes = 16 * 1024 * 1024

const (
	stdioProcessExitTimeout = 2 * time.Second
	stdioStderrDrainTimeout = 500 * time.Millisecond
	// The Linux backend consumes this internal launch profile before exec. It
	// asks bubblewrap for a directory-level Metis view, which remains safe when
	// OAuth stores are created after a long-lived stdio server has started.
	linuxStdioMCPSandboxProfile        = "METIS_INTERNAL_SANDBOX_PROFILE=stdio-mcp"
	linuxStdioMCPDesktopSandboxProfile = "METIS_INTERNAL_SANDBOX_PROFILE=stdio-mcp-desktop"
)

// StdioSandboxProfile is a host-selected capability profile for a local MCP
// subprocess. Configuration files cannot set it: callers must opt in through
// the typed constructor, and the private Linux launch marker is added only
// after user-provided environment values have been sanitized.
type StdioSandboxProfile uint8

const (
	StdioSandboxProfileGeneric StdioSandboxProfile = iota
	StdioSandboxProfileComputerUse
)

func readBoundedJSONRPC(r io.Reader) ([]byte, error) {
	limited := &io.LimitedReader{R: r, N: maxMCPMessageBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxMCPMessageBytes {
		return nil, fmt.Errorf("MCP JSON-RPC message exceeds %d-byte limit", maxMCPMessageBytes)
	}
	return data, nil
}

func newMCPMessageScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxMCPMessageBytes)
	return scanner
}

// StdioTransport launches a local MCP server and communicates over stdin/stdout.
type StdioTransport struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	closeOnce sync.Once
	closeErr  error
	// releaseSandbox drops the Manager lease acquired before process start.
	// Keeping it until the child and stderr drain have stopped prevents a
	// concurrent runtime shutdown from deleting the sandbox temp directory
	// out from under a live MCP/Computer Use process.
	releaseSandbox func()
	// stderrBuf is the bounded ring of stderr bytes the subprocess
	// emitted. We keep at most MaxStderrBytes — used to surface
	// real diagnostic output when a handshake / tool call fails
	// (without it, the user just sees "EOF" with no context).
	stderrBuf *boundedBuffer
	stderrWG  sync.WaitGroup
	// redactValues contains only values of explicitly configured
	// credential-shaped environment keys. Servers occasionally echo their
	// environment in startup errors; retaining these values lets Stderr and
	// JSON-RPC errors remove even opaque tokens that have no recognizable
	// provider prefix.
	redactValues []string
}

// NewStdioTransport starts an MCP server process and returns a transport over its stdio.
// Pipes are released on every error path so a failed Start doesn't leak fds.
func NewStdioTransport(ctx context.Context, command string, args ...string) (*StdioTransport, error) {
	return NewStdioTransportWithEnv(ctx, command, nil, args...)
}

// NewStdioTransportWithEnv is the env-aware variant. The subprocess receives
// a minimal launch environment plus the caller-supplied KEY=VAL entries. It
// never inherits unrelated provider credentials or user environment values.
func NewStdioTransportWithEnv(ctx context.Context, command string, extraEnv []string, args ...string) (*StdioTransport, error) {
	return NewStdioTransportWithEnvAndDir(ctx, command, extraEnv, "", args...)
}

// NewStdioTransportWithEnvAndDir is the plugin-aware stdio constructor. A
// non-empty workingDir is assigned directly to exec.Cmd.Dir, which lets
// translated Codex MCP bundles keep their package-relative commands and
// assets without changing the process-wide working directory.
func NewStdioTransportWithEnvAndDir(ctx context.Context, command string, extraEnv []string, workingDir string, args ...string) (*StdioTransport, error) {
	return NewStdioTransportWithEnvAndDirAndSandbox(ctx, command, extraEnv, workingDir, nil, args...)
}

// NewStdioTransportWithEnvAndDirAndSandbox starts a stdio MCP server through
// the runtime-owned process sandbox. The caller retains ownership of manager;
// it must outlive the returned transport and be closed only after all MCP
// transports have stopped. Explicit per-server environment values remain
// available inside the server, while sanitizedStdioEnv prevents unrelated
// process credentials from reaching it.
func NewStdioTransportWithEnvAndDirAndSandbox(ctx context.Context, command string, extraEnv []string, workingDir string, manager *sandbox.Manager, args ...string) (*StdioTransport, error) {
	return NewStdioTransportWithEnvAndDirAndSandboxProfile(ctx, command, extraEnv, workingDir, manager, StdioSandboxProfileGeneric, args...)
}

// NewStdioTransportWithEnvAndDirAndSandboxProfile is the capability-aware
// stdio constructor. Computer Use is intentionally not inferred from argv or
// executable basename; the trusted runtime must select the dedicated profile.
func NewStdioTransportWithEnvAndDirAndSandboxProfile(ctx context.Context, command string, extraEnv []string, workingDir string, manager *sandbox.Manager, profile StdioSandboxProfile, args ...string) (*StdioTransport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if profile != StdioSandboxProfileGeneric && profile != StdioSandboxProfileComputerUse {
		return nil, fmt.Errorf("mcp: invalid stdio sandbox profile %d", profile)
	}
	// Process lifetime is owned by StdioTransport.Close, not by the bounded
	// launch/handshake context. Slash commands and lazy first-tool spawns use
	// short operation contexts; binding exec.CommandContext to those contexts
	// would kill an otherwise healthy server immediately after launch.
	cmd := exec.Command(command, args...)
	configureStdioProcessTree(cmd)
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	// Always set Env. A nil exec.Cmd.Env inherits the complete METIS process
	// environment, including provider API keys that were never intended for
	// this MCP/Computer Use server.
	cmd.Env = sanitizedStdioEnvForProfile(extraEnv, workingDir, profile)
	var releaseSandbox func()
	if manager != nil {
		// sanitizedStdioEnv already limits the parent environment. Preserve the
		// explicitly configured server credentials while normalizing TMPDIR and
		// non-interactive markers to the shared sandbox boundary.
		cmd.Env = manager.FilterEnv(cmd.Env, true)
		req := sandbox.Request{Cwd: workingDir}
		// On supported hosts, local MCP and Computer Use processes always get
		// the credential-read boundary even when the user's ordinary sandbox
		// mode is off. This also makes a later switch to bypassPermissions safe:
		// the already-running server was never launched with broad host access.
		// Unsupported hosts preserve normal-mode compatibility; bypass itself
		// remains fail-closed through Manager.PreflightCredentialIsolation.
		if manager.Available() {
			req.MinimumMode = sandbox.ModePermissions
			if runtime.GOOS == "linux" {
				marker := linuxStdioMCPSandboxProfile
				if profile == StdioSandboxProfileComputerUse {
					marker = linuxStdioMCPDesktopSandboxProfile
				}
				cmd.Env = append(cmd.Env, marker)
			}
		}
		wrapped, release, err := manager.Acquire(cmd, req)
		if err != nil {
			return nil, fmt.Errorf("sandbox stdio MCP %s: %w", filepath.Base(command), err)
		}
		cmd = wrapped
		releaseSandbox = release
	}
	if err := ctx.Err(); err != nil {
		if releaseSandbox != nil {
			releaseSandbox()
		}
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		if releaseSandbox != nil {
			releaseSandbox()
		}
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		if releaseSandbox != nil {
			releaseSandbox()
		}
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		if releaseSandbox != nil {
			releaseSandbox()
		}
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		if releaseSandbox != nil {
			releaseSandbox()
		}
		return nil, fmt.Errorf("start %s: %w", command, err)
	}
	t := &StdioTransport{
		cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr,
		stderrBuf: newBoundedBuffer(MaxStderrBytes),
		redactValues: normalizeExactRedactionValues(append(
			configuredSecretEnvValues(extraEnv),
			configuredSecretArgValues(args)...,
		)),
		releaseSandbox: releaseSandbox,
	}
	// Drain stderr in a goroutine so the kernel pipe buffer never
	// fills (a full pipe would block the subprocess's next write,
	// freezing the whole server). The bounded writer silently
	// drops bytes past the cap.
	t.stderrWG.Add(1)
	go func() {
		defer t.stderrWG.Done()
		_, _ = io.Copy(t.stderrBuf, stderr)
	}()
	return t, nil
}

func (t *StdioTransport) Close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		if t.releaseSandbox != nil {
			defer t.releaseSandbox()
		}
		// Terminate the process tree while the leader PID is still available.
		// Closing stdin first lets a cooperative shell exit and orphan children
		// that inherited stderr; on Windows that also makes taskkill /T unable to
		// discover them. The tree termination is bounded per platform.
		if t.cmd != nil && t.cmd.Process != nil {
			terminateStdioProcessTree(t.cmd.Process, stdioProcessExitTimeout)
		}
		if t.stdin != nil {
			_ = t.stdin.Close()
		}
		if t.cmd != nil {
			waitDone := make(chan error, 1)
			go func() {
				waitDone <- t.cmd.Wait()
			}()
			timer := time.NewTimer(stdioProcessExitTimeout)
			select {
			case <-waitDone:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
				// A broken platform tree-kill must not make runtime cleanup hang.
				// Force the leader once more, close our pipe endpoints, and return a
				// bounded diagnostic even if the OS never lets Wait reap it.
				if t.cmd.Process != nil {
					_ = t.cmd.Process.Kill()
				}
				if t.stdout != nil {
					_ = t.stdout.Close()
				}
				if t.stderr != nil {
					_ = t.stderr.Close()
				}
				select {
				case <-waitDone:
				case <-time.After(stdioStderrDrainTimeout):
					t.closeErr = fmt.Errorf("mcp: stdio process did not exit within %s", stdioProcessExitTimeout+stdioStderrDrainTimeout)
				}
			}
		}
		if t.stdout != nil {
			_ = t.stdout.Close()
		}
		stderrDone := make(chan struct{})
		go func() {
			t.stderrWG.Wait()
			close(stderrDone)
		}()
		timer := time.NewTimer(stdioStderrDrainTimeout)
		select {
		case <-stderrDone:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			// A descendant may deliberately escape the process group while
			// retaining stderr. Closing our reader guarantees io.Copy unblocks;
			// the second wait is bounded as a final defense against a broken fd.
			if t.stderr != nil {
				_ = t.stderr.Close()
			}
			select {
			case <-stderrDone:
			case <-time.After(stdioStderrDrainTimeout):
				if t.closeErr == nil {
					t.closeErr = fmt.Errorf("mcp: stdio stderr did not drain within %s", 2*stdioStderrDrainTimeout)
				}
			}
		}
		if t.stderr != nil {
			_ = t.stderr.Close()
		}
	})
	return t.closeErr
}

// Stderr returns the captured (truncated) stderr output. Callers use
// this to attach real diagnostic context to handshake / RPC errors —
// a failed `npx some-mcp` is much easier to debug when "command not
// found: some-mcp" surfaces alongside the EOF error than when it
// disappeared into a black hole.
func (t *StdioTransport) Stderr() string {
	if t.stderrBuf == nil {
		return ""
	}
	guardBytes := maxExactRedactionValueBytes(t.redactValues)
	if guardBytes > 0 {
		guardBytes--
	}
	return t.redactSensitiveText(t.stderrBuf.stringWithTrailingDrop(guardBytes))
}

// stdioBaseEnvKeys is deliberately small. These variables are sufficient for
// locating child executables, home/cache discovery, temporary files, locale,
// and native Windows process startup. Network credentials, proxy settings,
// language injection variables (NODE_OPTIONS/PYTHONPATH), SSH agents, and
// provider API keys are excluded unless the MCP server explicitly configures
// them in its own env map.
var stdioBaseEnvKeys = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL",
	"TMPDIR", "TMP", "TEMP",
	"LANG", "LC_ALL", "LC_CTYPE", "TERM", "COLORTERM", "NO_COLOR", "CI",
	// Windows process launch and user-directory essentials.
	"PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "USERPROFILE",
	"HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "USERNAME",
}

var stdioDesktopEnvKeys = map[string]struct{}{
	"DISPLAY": {}, "WAYLAND_DISPLAY": {}, "XAUTHORITY": {},
	"XDG_RUNTIME_DIR": {}, "DBUS_SESSION_BUS_ADDRESS": {},
}

type stdioEnvEntry struct {
	key   string
	value string
}

// sanitizedStdioEnv builds a deterministic, minimal subprocess environment.
// Explicit server entries override inherited essentials with the same key.
func sanitizedStdioEnv(extraEnv []string, workingDir string) []string {
	return sanitizedStdioEnvForProfile(extraEnv, workingDir, StdioSandboxProfileGeneric)
}

func sanitizedStdioEnvForProfile(extraEnv []string, workingDir string, profile StdioSandboxProfile) []string {
	entries := make([]stdioEnvEntry, 0, len(stdioBaseEnvKeys)+len(extraEnv)+1)
	index := make(map[string]int, cap(entries))
	canonical := func(key string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(key)
		}
		return key
	}
	set := func(key, value string) {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return
		}
		upperKey := strings.ToUpper(key)
		if upperKey == "METIS_INTERNAL_SANDBOX_PROFILE" {
			return
		}
		if _, desktopHandle := stdioDesktopEnvKeys[upperKey]; desktopHandle {
			// Even explicit generic MCP configuration cannot inherit the host
			// desktop/session bus. The dedicated Computer Use profile needs only
			// X11 display selection and its authorization cookie.
			if profile != StdioSandboxProfileComputerUse || (upperKey != "DISPLAY" && upperKey != "XAUTHORITY") {
				return
			}
		}
		ck := canonical(key)
		if idx, ok := index[ck]; ok {
			entries[idx] = stdioEnvEntry{key: key, value: value}
			return
		}
		index[ck] = len(entries)
		entries = append(entries, stdioEnvEntry{key: key, value: value})
	}

	for _, key := range stdioBaseEnvKeys {
		if value, ok := os.LookupEnv(key); ok {
			set(key, value)
		}
	}
	if workingDir == "" {
		workingDir, _ = os.Getwd()
	}
	if workingDir != "" {
		set("PWD", workingDir)
	}
	for _, raw := range extraEnv {
		key, value, ok := strings.Cut(raw, "=")
		if !ok {
			continue
		}
		set(key, value)
	}
	// Force the same agent markers as Bash and sandboxed subprocesses after
	// applying explicit server env. MCP/Computer Use wrappers can use these to
	// suppress pagers, login prompts, editors, and other interactive behavior;
	// a server entry cannot override them back to an interactive identity.
	set("AGENT", "metis")
	set("AI_AGENT", "metis")
	set("METIS", "1")

	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.key+"="+entry.value)
	}
	return out
}

func configuredSecretEnvValues(extraEnv []string) []string {
	seen := make(map[string]struct{})
	var values []string
	for _, raw := range extraEnv {
		key, value, ok := strings.Cut(raw, "=")
		if !ok || !shouldRedactExplicitValue(key, value, false) {
			continue
		}
		// Every value admitted by shouldRedactExplicitValue is an explicit
		// credential boundary. Split a valid "Scheme payload" form regardless
		// of the field name because custom variables (for example FIRECRAWL_KEY)
		// are often echoed by child processes as the payload alone.
		values = appendCredentialRedactionValues(values, seen, value, true)
	}
	// Replace longer values first so overlapping values cannot leave a suffix.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

func configuredSecretArgValues(args []string) []string {
	seen := make(map[string]struct{})
	var values []string
	for i := 0; i < len(args); i++ {
		flag, inlineValue, hasInlineValue := splitCredentialArg(args[i])
		if !isCredentialArgFlag(flag) {
			continue
		}
		value := inlineValue
		if !hasInlineValue {
			if i+1 >= len(args) {
				continue
			}
			i++
			value = args[i]
		}
		values = appendCredentialRedactionValues(values, seen, value, true)
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

func splitCredentialArg(arg string) (flag, value string, hasValue bool) {
	trimmed := strings.TrimSpace(arg)
	if !strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "/") {
		return trimmed, "", false
	}
	for _, separator := range []string{"=", ":"} {
		if i := strings.Index(trimmed, separator); i > 0 {
			return trimmed[:i], trimmed[i+1:], true
		}
	}
	return trimmed, "", false
}

func isCredentialArgFlag(flag string) bool {
	normalized := strings.ToLower(strings.TrimLeft(strings.TrimSpace(flag), "-/"))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	switch normalized {
	case "api-key", "token", "secret", "password", "authorization", "credential", "license":
		return true
	default:
		return false
	}
}

func configuredSecretHeaderValues(headers map[string]string) []string {
	seen := make(map[string]struct{})
	values := make([]string, 0, len(headers))
	for key, value := range headers {
		// HTTP header names use '-' where environment variables commonly use
		// '_'. Normalize before reusing the credential-key classifier so names
		// such as X-API-Key, X-Session-Token, Authorization, and Cookie are
		// handled consistently.
		normalizedKey := strings.ReplaceAll(key, "-", "_")
		if !shouldRedactExplicitValue(normalizedKey, value, true) {
			continue
		}
		values = appendCredentialRedactionValues(values, seen, value, true)
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

func configuredSecretHTTPValues(t *HTTPTransport) []string {
	if t == nil {
		return nil
	}
	values := configuredSecretHeaderValues(t.headers)
	endpoint := strings.TrimSpace(t.endpoint)
	if endpoint != "" && looksCredentialBearingURL(endpoint) {
		seen := make(map[string]struct{}, len(values)+8)
		for _, value := range values {
			seen[value] = struct{}{}
		}
		for _, value := range credentialURLRedactionValues(endpoint, true) {
			values = appendCredentialRedactionValues(values, seen, value, false)
		}
		sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	}
	return values
}

func appendCredentialRedactionValues(values []string, seen map[string]struct{}, value string, splitSchemePayload bool) []string {
	add := func(candidate string) {
		if candidate == "" {
			return
		}
		if _, ok := seen[candidate]; ok {
			return
		}
		seen[candidate] = struct{}{}
		values = append(values, candidate)
	}
	add(value)
	trimmed := strings.TrimSpace(value)
	add(trimmed)

	// Credential scheme names are extensible (GNAP, DPoP, vendor-specific
	// schemes, ...). Retain the payload after the first RFC-token scheme as an
	// exact value too: upstreams often log it without the scheme.
	if splitSchemePayload {
		if i := strings.IndexAny(trimmed, " \t"); i > 0 && validAuthScheme(trimmed[:i]) {
			add(strings.TrimSpace(trimmed[i+1:]))
		}
	}
	return values
}

func validAuthScheme(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}

func isCredentialEnvKey(key string) bool {
	upper := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(key))
	for _, marker := range []string{
		"API_KEY", "APIKEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD",
		"CREDENTIAL", "AUTHORIZATION", "PRIVATE_KEY", "ACCESS_KEY", "COOKIE",
		"WEBHOOK", "SIGNED_URL", "LICENSE",
	} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	// Common custom names do not include API_KEY/AUTHORIZATION. Keep the
	// suffix check narrow so unrelated words such as MONKEY do not match.
	if upper == "KEY" || strings.HasPrefix(upper, "KEY_") ||
		strings.Contains(upper, "_KEY_") || strings.HasSuffix(upper, "_KEY") ||
		upper == "AUTH" || strings.HasPrefix(upper, "AUTH_") ||
		strings.Contains(upper, "_AUTH_") || strings.HasSuffix(upper, "_AUTH") ||
		strings.Contains(upper, "OAUTH") {
		return true
	}
	return false
}

const minOpaqueExplicitValueBytes = 12

func shouldRedactExplicitValue(key, value string, header bool) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	if isCredentialEnvKey(key) {
		return true
	}
	// Exact matching makes false positives unlikely, but very short values
	// such as "true", "3000", or "dev" occur throughout normal tool output.
	if len(trimmed) < minOpaqueExplicitValueBytes {
		return false
	}
	if header {
		if isObviouslyNonSecretHeaderKey(key) {
			return false
		}
	} else if isObviouslyNonSecretEnvKey(key) {
		return false
	}
	if isObviouslyNonSecretLocation(trimmed) {
		return false
	}
	return true
}

func isObviouslyNonSecretHeaderKey(key string) bool {
	switch strings.ToUpper(strings.ReplaceAll(key, "-", "_")) {
	case "ACCEPT", "ACCEPT_LANGUAGE", "CONTENT_TYPE", "USER_AGENT", "ORIGIN", "REFERER",
		"X_TRACE_ID", "TRACEPARENT", "TRACESTATE", "X_REQUEST_ID", "REQUEST_ID", "CORRELATION_ID":
		return true
	default:
		return false
	}
}

func isObviouslyNonSecretEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, allowed := range stdioBaseEnvKeys {
		if upper == strings.ToUpper(allowed) {
			return true
		}
	}
	switch upper {
	case "PWD", "OLDPWD", "WORKSPACE", "WORKSPACE_ROOT", "PROJECT_ROOT",
		"NODE_ENV", "ENV", "ENVIRONMENT", "DEBUG", "LOG_LEVEL", "PORT", "HOST",
		"AGENT", "AI_AGENT", "METIS":
		return true
	default:
		return false
	}
}

func isObviouslyNonSecretLocation(value string) bool {
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "./") ||
		strings.HasPrefix(value, "../") || strings.HasPrefix(value, "~/") ||
		strings.HasPrefix(value, `\`) {
		return true
	}
	if len(value) >= 3 && value[1] == ':' &&
		((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) &&
		(value[2] == '\\' || value[2] == '/') {
		return true
	}
	lower := strings.ToLower(value)
	for _, prefix := range []string{"http://", "https://", "ws://", "wss://", "file://"} {
		if strings.HasPrefix(lower, prefix) {
			return !looksCredentialBearingURL(value)
		}
	}
	return false
}

func looksCredentialBearingURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return true
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return true
	}
	host := strings.ToLower(u.Hostname())
	path := strings.ToLower(u.EscapedPath())
	if strings.HasPrefix(host, "hooks.") || strings.Contains(path, "/webhook") || strings.Contains(path, "/hooks/") {
		return true
	}
	for _, segment := range strings.Split(u.EscapedPath(), "/") {
		segment, _ = url.PathUnescape(segment)
		if looksOpaqueURLSegment(segment) {
			return true
		}
	}
	return false
}

// credentialURLRedactionValues extracts independently echoable credential
// components from a URI. Redacting only the complete URI is insufficient:
// HTTP stacks and MCP servers commonly log a userinfo password, signed query
// value, webhook path token, or fragment without the surrounding endpoint.
func credentialURLRedactionValues(raw string, includeWhole bool) []string {
	seen := make(map[string]struct{})
	values := make([]string, 0, 12)
	add := func(value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}

	trimmed := strings.TrimSpace(raw)
	if includeWhole {
		add(trimmed)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
		return values
	}
	if u.User != nil {
		username := u.User.Username()
		if len(username) >= minOpaqueExplicitValueBytes || looksOpaqueURLSegment(username) {
			add(username)
			add(url.QueryEscape(username))
			add(url.PathEscape(username))
		}
		if password, ok := u.User.Password(); ok {
			add(password)
			add(url.QueryEscape(password))
			add(url.PathEscape(password))
		}
	}

	// Preserve both encoded and decoded query forms. The decoded value is what
	// application errors usually print; reverse proxies often print RawQuery.
	for _, pair := range strings.Split(u.RawQuery, "&") {
		if pair == "" {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(pair, "=")
		key, keyErr := url.QueryUnescape(rawKey)
		value, valueErr := url.QueryUnescape(rawValue)
		if keyErr != nil {
			key = rawKey
		}
		if valueErr != nil {
			value = rawValue
		}
		normalizedKey := strings.NewReplacer("-", "_", ".", "_").Replace(key)
		if rawValue == "" && looksOpaqueURLSegment(key) {
			add(key)
			add(rawKey)
			add(url.QueryEscape(key))
			add(url.PathEscape(key))
			continue
		}
		if isSensitiveURLQueryKey(normalizedKey) || looksOpaqueURLSegment(value) {
			add(value)
			add(rawValue)
			add(url.QueryEscape(value))
			add(url.PathEscape(value))
		}
	}

	hookLike := strings.HasPrefix(strings.ToLower(u.Hostname()), "hooks.") ||
		strings.Contains(strings.ToLower(u.EscapedPath()), "/webhook") ||
		strings.Contains(strings.ToLower(u.EscapedPath()), "/hooks/")
	pathSegments := strings.Split(u.EscapedPath(), "/")
	for i, escaped := range pathSegments {
		segment, err := url.PathUnescape(escaped)
		if err != nil {
			segment = escaped
		}
		precededByCredentialLabel := i > 0 && isSensitiveURLPathLabel(pathSegments[i-1])
		if looksOpaqueURLSegment(segment) ||
			((hookLike || precededByCredentialLabel) && len(segment) >= minOpaqueExplicitValueBytes) {
			add(segment)
			add(escaped)
			add(url.QueryEscape(segment))
			add(url.PathEscape(segment))
		}
	}
	if u.Fragment != "" {
		add(u.Fragment)
		add(u.RawFragment)
		add(url.QueryEscape(u.Fragment))
		add(url.PathEscape(u.Fragment))
	}

	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

func isSensitiveURLPathLabel(segment string) bool {
	segment, _ = url.PathUnescape(segment)
	switch strings.ToLower(strings.TrimSpace(segment)) {
	case "auth", "authorization", "credential", "key", "secret", "session", "sig",
		"signature", "signed", "token", "webhook":
		return true
	default:
		return false
	}
}

func isSensitiveURLQueryKey(key string) bool {
	upper := strings.ToUpper(key)
	if isCredentialEnvKey(upper) {
		return true
	}
	switch upper {
	case "SIG", "SIGNATURE", "CODE", "SAS", "X_AMZ_SIGNATURE", "X_GOOG_SIGNATURE",
		"X_AMZ_CREDENTIAL", "X_AMZ_SECURITY_TOKEN":
		return true
	default:
		return strings.HasSuffix(upper, "_SIGNATURE") || strings.HasSuffix(upper, "_SIG")
	}
}

func looksOpaqueURLSegment(segment string) bool {
	if len(segment) < 24 {
		return false
	}
	var letters, digits, lower, upper, punctuation int
	unique := make(map[rune]struct{}, 32)
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z':
			letters++
			lower++
		case r >= 'A' && r <= 'Z':
			letters++
			upper++
		case r >= '0' && r <= '9':
			digits++
		case r == '-' || r == '_' || r == '=':
			punctuation++
		default:
			return false
		}
		unique[r] = struct{}{}
	}
	// Most opaque identifiers mix letters and digits. Base64/base64url tokens
	// can also be alphabet-only, however, so retain a bounded high-diversity
	// fallback. Ordinary documentation slugs usually have repeated lowercase
	// words and remain below this threshold.
	if letters >= 4 && digits >= 4 {
		return true
	}
	if len(unique) >= 12 && (punctuation > 0 || (lower > 0 && upper > 0)) {
		return true
	}
	return len(segment) >= 32 && len(unique) >= 16
}

type textRedactor struct {
	exact *strings.Replacer
}

func newTextRedactor(exactValues []string) textRedactor {
	values := normalizeExactRedactionValues(exactValues)
	if len(values) == 0 {
		return textRedactor{}
	}
	pairs := make([]string, 0, len(values)*2)
	for _, value := range values {
		pairs = append(pairs, value, "[REDACTED]")
	}
	return textRedactor{exact: strings.NewReplacer(pairs...)}
}

func (r textRedactor) Redact(text string) string {
	if r.exact != nil {
		text = r.exact.Replace(text)
	}
	return security.RedactSubprocessText(text)
}

func redactSensitiveText(text string, exactValues []string) string {
	return newTextRedactor(exactValues).Redact(text)
}

func normalizeExactRedactionValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if len(normalized[i]) == len(normalized[j]) {
			return normalized[i] < normalized[j]
		}
		return len(normalized[i]) > len(normalized[j])
	})
	return normalized
}

func (t *StdioTransport) redactSensitiveText(text string) string {
	return redactSensitiveText(text, t.redactValues)
}

func maxExactRedactionValueBytes(values []string) int {
	maxLen := 0
	for _, value := range values {
		if len(value) > maxLen {
			maxLen = len(value)
		}
	}
	return maxLen
}

// boundedBuffer is a ring-style cap on accumulated bytes. Writes past
// `cap` are dropped silently and `truncated` records how many bytes
// were lost so the rendered output can show a "…(N bytes elided)"
// suffix.
type boundedBuffer struct {
	mu        sync.Mutex
	buf       []byte
	cap       int
	truncated int
}

func newBoundedBuffer(cap int) *boundedBuffer {
	return &boundedBuffer{cap: cap}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	room := b.cap - len(b.buf)
	if room <= 0 {
		b.truncated += len(p)
		return len(p), nil // pretend success so io.Copy keeps draining
	}
	if len(p) > room {
		b.buf = append(b.buf, p[:room]...)
		b.truncated += len(p) - room
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	return b.stringWithTrailingDrop(0)
}

func (b *boundedBuffer) stringWithTrailingDrop(dropOnTruncate int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated == 0 {
		return string(b.buf)
	}
	retained := b.buf
	if dropOnTruncate > len(retained) {
		dropOnTruncate = len(retained)
	}
	if dropOnTruncate > 0 {
		retained = retained[:len(retained)-dropOnTruncate]
	}
	return fmt.Sprintf("%s\n…(%d bytes elided after stderr cap)\n", string(retained), b.truncated+dropOnTruncate)
}

// JSONRPCRequest is a JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      interface{}     `json:"id,omitempty"`
}

// JSONRPCResponse is a JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
	ID      interface{}     `json:"id,omitempty"`
}

// JSONRPCError is a JSON-RPC 2.0 error.
type JSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Notification is an incoming JSON-RPC notification (no id).
type Notification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Tool describes an MCP tool available on the server.
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// ListToolsResult is the result of tools/list.
type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

// Client is an MCP client. It maintains a connection to one MCP server.
type Client struct {
	transport     Transport
	mu            sync.RWMutex
	pending       map[string]chan *JSONRPCResponse // request id → response chan
	notifications chan Notification
	ctx           context.Context
	cancel        context.CancelFunc
	closeOnce     sync.Once
	closeErr      error

	// idSeq generates monotonically increasing request ids. Using a counter
	// instead of time.UnixNano() avoids collisions when two send() calls
	// land in the same nanosecond.
	idSeq atomic.Uint64

	// writeMu serializes writes to the stdio transport's stdin. send(),
	// sendNotification(), and writeServerEnvelope() can all write
	// concurrently (the last from the handleServerRequest goroutine);
	// without this, two JSON-RPC frames interleave on the pipe and the
	// server's decoder chokes. HTTP transports write independent requests
	// so they don't need it.
	writeMu sync.Mutex

	// roots are the workspace roots advertised to servers and returned
	// when a server sends a roots/list request. Defaults to cwd.
	roots []Root
	// sampler, when set, fulfills server-initiated sampling/createMessage
	// requests by generating with the host LLM. nil → the client doesn't
	// advertise the sampling capability and declines such requests.
	sampler SamplingHandler

	// Resource URIs may themselves contain database credentials, signed query
	// strings, or private paths. Model-facing lists therefore expose only
	// per-client opaque handles. The bounded bidirectional map is cleared on
	// Close; handles never encode the original URI.
	resourceMu        sync.RWMutex
	resourceHandleKey [32]byte
	resourceByHandle  map[string]string
	resourceByURI     map[string]string
	resourceOrder     []string
	resourceURIBytes  int
	resourceClosed    bool

	// serverRequestSlots bounds server-initiated roots/sampling/ping work.
	// An untrusted server otherwise creates one goroutine per request frame.
	serverRequestSlots chan struct{}
}

const (
	resourceHandlePrefix        = "metis-resource://"
	maxResourceHandles          = 4096
	maxResourceURIBytes         = 16 * 1024
	maxResourceURITotalBytes    = 8 * 1024 * 1024
	maxConcurrentServerRequests = 16
)

// RedactText removes recognizable secrets plus exact credentials configured
// for this client's transport. MCP response text is untrusted: a server may
// echo its subprocess environment or an upstream Authorization header in an
// otherwise successful tool result.
func (c *Client) RedactText(text string) string {
	c.mu.RLock()
	transport := c.transport
	c.mu.RUnlock()
	switch t := transport.(type) {
	case *StdioTransport:
		return t.redactSensitiveText(text)
	case *HTTPTransport:
		return redactSensitiveText(text, configuredSecretHTTPValues(t))
	default:
		return security.RedactSubprocessText(text)
	}
}

func (c *Client) redactTextWithExact(text string, extraValues ...string) string {
	return c.newTextRedactor(extraValues...).Redact(text)
}

func (c *Client) newTextRedactor(extraValues ...string) textRedactor {
	c.mu.RLock()
	transport := c.transport
	c.mu.RUnlock()
	values := append([]string(nil), extraValues...)
	switch t := transport.(type) {
	case *StdioTransport:
		values = append(values, t.redactValues...)
	case *HTTPTransport:
		values = append(values, configuredSecretHTTPValues(t)...)
	}
	return newTextRedactor(values)
}

func (c *Client) redactError(err error) error {
	if err == nil {
		return nil
	}
	return &redactedTransportError{
		message:        c.RedactText(err.Error()),
		classification: safeErrorClassification(err),
	}
}

func (c *Client) containsSensitiveText(text string) bool {
	return text != "" && c.RedactText(text) != text
}

// redactJSONStrings recursively rewrites string values. A sensitive map key
// is structural (for example a JSON Schema property identifier), so rewriting
// it would silently change the protocol. Drop such entries fail-closed.
func (c *Client) redactJSONStrings(value any) any {
	switch v := value.(type) {
	case string:
		return c.RedactText(v)
	case []any:
		for i := range v {
			v[i] = c.redactJSONStrings(v[i])
		}
		return v
	case map[string]any:
		for key, item := range v {
			if c.containsSensitiveText(key) {
				delete(v, key)
				continue
			}
			if key == "required" {
				v[key] = c.dropSensitiveIdentifiers(item)
				continue
			}
			if key == "propertyName" {
				if identifier, ok := item.(string); ok && c.containsSensitiveText(identifier) {
					delete(v, key)
					continue
				}
			}
			v[key] = c.redactJSONStrings(item)
		}
		return v
	case []string:
		for i := range v {
			v[i] = c.RedactText(v[i])
		}
		return v
	case map[string]string:
		for key, item := range v {
			if c.containsSensitiveText(key) {
				delete(v, key)
				continue
			}
			v[key] = c.RedactText(item)
		}
		return v
	default:
		return value
	}
}

func (c *Client) dropSensitiveIdentifiers(value any) any {
	switch identifiers := value.(type) {
	case []any:
		safe := make([]any, 0, len(identifiers))
		for _, identifier := range identifiers {
			if text, ok := identifier.(string); ok && c.containsSensitiveText(text) {
				continue
			}
			safe = append(safe, c.redactJSONStrings(identifier))
		}
		return safe[:len(safe):len(safe)]
	case []string:
		safe := make([]string, 0, len(identifiers))
		for _, identifier := range identifiers {
			if !c.containsSensitiveText(identifier) {
				safe = append(safe, identifier)
			}
		}
		return safe[:len(safe):len(safe)]
	default:
		return c.redactJSONStrings(value)
	}
}

func (c *Client) resourceHandle(uri string) (string, bool) {
	uri = strings.TrimSpace(uri)
	if uri == "" || len(uri) > maxResourceURIBytes {
		return "", false
	}
	c.resourceMu.Lock()
	defer c.resourceMu.Unlock()
	return c.resourceHandleLocked(uri)
}

func (c *Client) resourceHandleLocked(uri string) (string, bool) {
	if c.resourceClosed {
		return "", false
	}
	if handle, ok := c.resourceByURI[uri]; ok {
		return handle, true
	}
	mac := hmac.New(sha256.New, c.resourceHandleKey[:])
	_, _ = mac.Write([]byte(uri))
	handle := resourceHandlePrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:18])
	if original, collision := c.resourceByHandle[handle]; collision && original != uri {
		return "", false
	}
	if len(c.resourceOrder) >= maxResourceHandles {
		oldest := c.resourceOrder[0]
		c.resourceOrder = c.resourceOrder[1:]
		if oldURI, ok := c.resourceByHandle[oldest]; ok {
			delete(c.resourceByURI, oldURI)
			c.resourceURIBytes -= len(oldURI)
		}
		delete(c.resourceByHandle, oldest)
	}
	for c.resourceURIBytes+len(uri) > maxResourceURITotalBytes && len(c.resourceOrder) > 0 {
		oldest := c.resourceOrder[0]
		c.resourceOrder = c.resourceOrder[1:]
		if oldURI, ok := c.resourceByHandle[oldest]; ok {
			delete(c.resourceByURI, oldURI)
			c.resourceURIBytes -= len(oldURI)
		}
		delete(c.resourceByHandle, oldest)
	}
	if c.resourceURIBytes+len(uri) > maxResourceURITotalBytes {
		return "", false
	}
	c.resourceByHandle[handle] = uri
	c.resourceByURI[uri] = handle
	c.resourceOrder = append(c.resourceOrder, handle)
	c.resourceURIBytes += len(uri)
	return handle, true
}

// resourceHandleLockedNoEvict adds a URI only when doing so cannot invalidate
// an existing model-visible handle. It is used while preparing a single
// resources/read response: response content may be arbitrarily large, but no
// handle returned in that response (or its listed parent) may already be dead
// when the call returns.
func (c *Client) resourceHandleLockedNoEvict(uri string) (string, bool) {
	if c.resourceClosed || uri == "" || len(uri) > maxResourceURIBytes {
		return "", false
	}
	if handle, ok := c.resourceByURI[uri]; ok {
		return handle, true
	}
	if len(c.resourceOrder) >= maxResourceHandles ||
		c.resourceURIBytes+len(uri) > maxResourceURITotalBytes {
		return "", false
	}
	mac := hmac.New(sha256.New, c.resourceHandleKey[:])
	_, _ = mac.Write([]byte(uri))
	handle := resourceHandlePrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:18])
	if original, collision := c.resourceByHandle[handle]; collision && original != uri {
		return "", false
	}
	c.resourceByHandle[handle] = uri
	c.resourceByURI[uri] = handle
	c.resourceOrder = append(c.resourceOrder, handle)
	c.resourceURIBytes += len(uri)
	return handle, true
}

func (c *Client) resolveResourceHandle(handle string) (string, error) {
	if !strings.HasPrefix(handle, resourceHandlePrefix) {
		return "", errors.New("MCP resource must use an opaque handle returned by ListMcpResources")
	}
	c.resourceMu.RLock()
	uri, ok := c.resourceByHandle[handle]
	c.resourceMu.RUnlock()
	if !ok {
		return "", errors.New("unknown or expired MCP resource handle; list resources again")
	}
	return uri, nil
}

func (c *Client) clearResourceHandles() {
	c.resourceMu.Lock()
	defer c.resourceMu.Unlock()
	clear(c.resourceByHandle)
	clear(c.resourceByURI)
	c.resourceOrder = nil
	c.resourceURIBytes = 0
	c.resourceClosed = true
}

func (c *Client) redactJSONRawStrings(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) > maxMCPMessageBytes {
		return nil, fmt.Errorf("MCP JSON payload exceeds %d-byte limit", maxMCPMessageBytes)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid MCP JSON payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("invalid MCP JSON payload: multiple values")
		}
		return nil, fmt.Errorf("invalid MCP JSON payload: %w", err)
	}
	redacted, err := json.Marshal(c.redactJSONStrings(value))
	if err != nil {
		return nil, fmt.Errorf("redact MCP JSON payload: %w", err)
	}
	if len(redacted) > maxMCPMessageBytes {
		return nil, fmt.Errorf("redacted MCP JSON payload exceeds %d-byte limit", maxMCPMessageBytes)
	}
	return redacted, nil
}

// mcpResponseError converts a server-provided JSON-RPC error into a local
// error only after removing recognizable and explicitly configured secrets.
func (c *Client) mcpResponseError(rpcErr *JSONRPCError) error {
	if rpcErr == nil {
		return nil
	}
	return fmt.Errorf("MCP error %d: %s", rpcErr.Code, c.RedactText(rpcErr.Message))
}

func (c *Client) mcpResponseErrorWithExact(rpcErr *JSONRPCError, exactValues ...string) error {
	if rpcErr == nil {
		return nil
	}
	message := c.RedactText(rpcErr.Message)
	message = redactSensitiveText(message, exactValues)
	return fmt.Errorf("MCP error %d: %s", rpcErr.Code, message)
}

// Root is a workspace root advertised to MCP servers (roots/list).
type Root struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// SamplingHandler fulfills a server-initiated sampling/createMessage
// request: given the raw params, it returns the raw JSON result (a
// CreateMessageResult) or an error. Wired by the runtime to the host
// provider; nil disables sampling.
type SamplingHandler func(ctx context.Context, params json.RawMessage) (json.RawMessage, error)

// SetRoots overrides the advertised workspace roots. Call before the
// initialize handshake (i.e. before NewClient runs it) to have the
// capability reflected; the roots/list responder uses the latest value
// regardless.
func (c *Client) SetRoots(roots []Root) {
	c.mu.Lock()
	c.roots = append([]Root(nil), roots...)
	c.mu.Unlock()
}

// SetSamplingHandler wires server-initiated sampling to the host LLM.
// Call before NewClient's initialize so the capability is advertised.
func (c *Client) SetSamplingHandler(h SamplingHandler) { c.sampler = h }

// defaultRoots returns the cwd as the single advertised root.
func defaultRoots() []Root {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return nil
	}
	return []Root{{URI: "file://" + wd, Name: filepathBase(wd)}}
}

// filepathBase is a tiny basename to avoid importing path/filepath just
// for the root Name.
func filepathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// NewClient creates an MCP client using the given transport.
func NewClient(ctx context.Context, transport Transport) *Client {
	ctx, cancel := context.WithCancel(ctx)
	c := &Client{
		transport:          transport,
		pending:            make(map[string]chan *JSONRPCResponse),
		notifications:      make(chan Notification, 50),
		ctx:                ctx,
		cancel:             cancel,
		roots:              defaultRoots(),
		resourceByHandle:   make(map[string]string),
		resourceByURI:      make(map[string]string),
		serverRequestSlots: make(chan struct{}, maxConcurrentServerRequests),
	}
	if _, err := rand.Read(c.resourceHandleKey[:]); err != nil {
		// crypto/rand failure should not force raw resource URIs across the
		// model boundary. A process-local fallback still produces opaque handles.
		c.resourceHandleKey = sha256.Sum256([]byte(fmt.Sprintf("%d:%p", time.Now().UnixNano(), transport)))
	}
	return c
}

// NewStdioClient starts an MCP server subprocess and connects to it.
//
// Handshake (the initial tools/list) gets ConnectTimeout regardless of
// the caller's ctx deadline — this is a tighter budget than the post-
// connect RequestTimeout. Without an explicit short clock here, a
// hanging server keeps `/mcp start` blocked the full RequestTimeout
// or longer (cmd_cu's caller-set 30 s used to be the only stop-gap).
func NewStdioClient(ctx context.Context, command string, args ...string) (*Client, error) {
	return NewStdioClientWithEnv(ctx, command, nil, args...)
}

// NewStdioClientWithEnv is the env-aware variant of NewStdioClient. The
// `extraEnv` entries (KEY=VAL strings) augment the sanitized launch
// environment for this server only.
func NewStdioClientWithEnv(ctx context.Context, command string, extraEnv []string, args ...string) (*Client, error) {
	return NewStdioClientWithEnvAndDir(ctx, command, extraEnv, "", args...)
}

// NewStdioClientWithEnvAndDir starts a stdio MCP server inside the declared
// plugin working directory.
func NewStdioClientWithEnvAndDir(ctx context.Context, command string, extraEnv []string, workingDir string, args ...string) (*Client, error) {
	return NewStdioClientWithEnvAndDirAndSandbox(ctx, command, extraEnv, workingDir, nil, args...)
}

// NewStdioClientWithEnvAndDirAndSandbox is the runtime-sandboxed variant of
// NewStdioClientWithEnvAndDir. manager remains owned by the caller.
func NewStdioClientWithEnvAndDirAndSandbox(ctx context.Context, command string, extraEnv []string, workingDir string, manager *sandbox.Manager, args ...string) (*Client, error) {
	return NewStdioClientWithEnvAndDirAndSandboxProfile(ctx, command, extraEnv, workingDir, manager, StdioSandboxProfileGeneric, args...)
}

// NewStdioClientWithEnvAndDirAndSandboxProfile is the capability-aware client
// constructor used by the reserved Computer Use server path.
func NewStdioClientWithEnvAndDirAndSandboxProfile(ctx context.Context, command string, extraEnv []string, workingDir string, manager *sandbox.Manager, profile StdioSandboxProfile, args ...string) (*Client, error) {
	transport, err := NewStdioTransportWithEnvAndDirAndSandboxProfile(ctx, command, extraEnv, workingDir, manager, profile, args...)
	if err != nil {
		return nil, err
	}
	// The caller's ctx bounds only initialization. The returned client owns a
	// separate lifecycle that ends at Client.Close, so a completed `/mcp start`
	// or first lazy tool call does not tear down the live server.
	c := NewClient(context.Background(), transport)
	go c.readLoop()
	handshakeCtx, cancel := context.WithTimeout(ctx, ConnectTimeout())
	defer cancel()
	if err := c.initialize(handshakeCtx); err != nil {
		c.Close()
		if stderr := transport.Stderr(); stderr != "" {
			return nil, fmt.Errorf("MCP initialize: %w\nserver stderr:\n%s", err, stderr)
		}
		return nil, fmt.Errorf("MCP initialize: %w", err)
	}
	if _, err := c.ListTools(handshakeCtx); err != nil {
		c.Close()
		// Surface stderr from the subprocess so the user sees the
		// real reason (not just "EOF" / "timeout"). Bounded by
		// MaxStderrBytes — see boundedBuffer in NewStdioTransport.
		if stderr := transport.Stderr(); stderr != "" {
			return nil, fmt.Errorf("MCP handshake: %w\nserver stderr:\n%s", err, stderr)
		}
		return nil, fmt.Errorf("MCP handshake: %w", err)
	}
	return c, nil
}

// NewHTTPClient connects to a remote MCP server using the modern
// "Streamable HTTP" transport. The client POSTs JSON-RPC requests to
// `endpoint`; the server responds either with a single JSON body
// (`Content-Type: application/json`) or with an SSE stream
// (`Content-Type: text/event-stream`) carrying one or more responses.
// In addition we open a long-lived GET on the same endpoint with
// `Accept: text/event-stream` so server-initiated notifications
// (e.g. tools/list_changed) reach `c.notifications`.
//
// optHeaders lets callers attach auth (`Authorization: Bearer …`) or
// API-key headers — passed straight through on every request.
func NewHTTPClient(ctx context.Context, endpoint string, optHeaders ...map[string]string) (*Client, error) {
	var headers map[string]string
	if len(optHeaders) > 0 {
		headers = make(map[string]string, len(optHeaders[0]))
		for key, value := range optHeaders[0] {
			headers[key] = value
		}
	}
	transport := &HTTPTransport{
		endpoint: endpoint,
		client:   newMCPHTTPClient(),
		headers:  headers,
	}
	// As with stdio, the operation context bounds only the HTTP handshake.
	// Runtime ownership and Client.Close control the long-lived SSE/client
	// lifecycle after a successful connection.
	c := NewClient(context.Background(), transport)
	go c.httpNotificationLoop()
	handshakeCtx, cancel := context.WithTimeout(ctx, ConnectTimeout())
	defer cancel()
	if err := c.initialize(handshakeCtx); err != nil {
		c.Close()
		return nil, fmt.Errorf("MCP HTTP initialize: %w", err)
	}
	if _, err := c.ListTools(handshakeCtx); err != nil {
		c.Close()
		return nil, fmt.Errorf("MCP HTTP handshake: %w", err)
	}
	return c, nil
}

// Close terminates the MCP server connection.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		c.clearResourceHandles()
		if c.transport != nil {
			c.closeErr = c.transport.Close()
		}
	})
	return c.closeErr
}

// send issues a JSON-RPC request and waits for its response.
//
// Three things this guards against (each one was a real bug):
//  1. id collision when two callers hit the same nanosecond — we use a
//     monotonic counter, not time.UnixNano().
//  2. stdin write failure being swallowed — if the subprocess died, we
//     surface the write error immediately instead of waiting for ctx.
//  3. pending[id] leaking when ctx is cancelled or HTTP returns directly
//     — every exit path through this function deletes the entry.
func (c *Client) send(ctx context.Context, method string, params interface{}) (*JSONRPCResponse, error) {
	// Omit params entirely when nil rather than sending `"params":null`.
	// Spec-strict servers (the official @modelcontextprotocol/sdk) validate
	// the JSON-RPC envelope with a schema where params is optional-not-
	// nullable, and reject a literal null with -32700. Leaving raw empty
	// lets the omitempty tag drop the field.
	var raw json.RawMessage
	if params != nil {
		var err error
		raw, err = json.Marshal(params)
		if err != nil {
			return nil, err
		}
	}
	id := fmt.Sprintf("%d", c.idSeq.Add(1))
	ch := make(chan *JSONRPCResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	req := JSONRPCRequest{JSONRPC: "2.0", Method: method, Params: raw, ID: id}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	c.mu.RLock()
	transport := c.transport
	c.mu.RUnlock()

	switch t := transport.(type) {
	case *StdioTransport:
		c.writeMu.Lock()
		_, werr := t.stdin.Write(append(data, '\n'))
		c.writeMu.Unlock()
		if werr != nil {
			return nil, fmt.Errorf("mcp stdio write: %w", werr)
		}
	case *HTTPTransport:
		// Streamable HTTP: POST the request, then look at
		// Content-Type to choose the response shape.
		//   - application/json : single response, decode + return
		//   - text/event-stream: SSE frames; the response we want
		//     comes through as a `data:` line carrying the JSON-RPC
		//     reply matching our id. The server may send notifications
		//     in the same stream — those get routed via dispatchJSONRPC
		//     into c.notifications.
		req, rerr := http.NewRequestWithContext(ctx, "POST", t.endpoint, strings.NewReader(string(data)))
		if rerr != nil {
			return nil, c.redactError(rerr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
		resp, err := t.do(req)
		if err != nil {
			return nil, c.redactError(err)
		}
		defer resp.Body.Close()
		ct := resp.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "text/event-stream") {
			// SSE response — feed lines through the same dispatcher
			// path; the matching response will arrive on `ch` because
			// we registered into c.pending above.
			parseDone := make(chan error, 1)
			go func() { parseDone <- c.parseSSE(resp.Body) }()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-c.ctx.Done():
				return nil, ErrTransportClosed
			case rpcResp := <-ch:
				return rpcResp, nil
			case parseErr := <-parseDone:
				if parseErr != nil {
					return nil, c.redactError(parseErr)
				}
				return nil, errors.New("MCP SSE response ended before matching JSON-RPC response")
			}
		}
		body, err := readBoundedJSONRPC(resp.Body)
		if err != nil {
			return nil, c.redactError(err)
		}
		var rpcResp JSONRPCResponse
		if err := json.Unmarshal(body, &rpcResp); err != nil {
			return nil, c.redactError(err)
		}
		return &rpcResp, nil
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, ErrTransportClosed
	case resp := <-ch:
		return resp, nil
	}
}

// MCPProtocolVersion is the MCP version metis advertises in the
// initialize handshake. 2024-11-05 is the Streamable-HTTP baseline the
// official SDK servers accept.
const MCPProtocolVersion = "2024-11-05"

// initialize performs the MCP lifecycle handshake the spec requires
// before any other request: an `initialize` request followed by an
// `notifications/initialized` notification. metis historically skipped
// this and used tools/list as a de-facto handshake — lenient servers
// tolerated it, but spec-strict ones (the official @modelcontextprotocol
// SDK) reject the out-of-order tools/list. Called once by the client
// constructors before the initial ListTools.
func (c *Client) initialize(ctx context.Context) error {
	// Advertise the client capabilities the server may rely on:
	//   - roots: we answer roots/list with the workspace roots.
	//   - sampling: only when a host-LLM sampler is wired, so servers
	//     don't send sampling/createMessage we can't fulfill.
	caps := map[string]any{
		"roots": map[string]any{"listChanged": false},
	}
	if c.sampler != nil {
		caps["sampling"] = map[string]any{}
	}
	params := map[string]any{
		"protocolVersion": MCPProtocolVersion,
		"capabilities":    caps,
		"clientInfo":      map[string]any{"name": "metis", "version": "0.1"},
	}
	resp, err := c.send(ctx, "initialize", params)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return c.mcpResponseError(resp.Error)
	}
	// Best-effort initialized notification; servers don't reply to it.
	return c.sendNotification(ctx, "notifications/initialized", nil)
}

// sendNotification writes a fire-and-forget JSON-RPC notification (no id,
// no response awaited). Used for notifications/initialized.
func (c *Client) sendNotification(ctx context.Context, method string, params interface{}) error {
	var raw json.RawMessage
	if params != nil {
		var err error
		raw, err = json.Marshal(params)
		if err != nil {
			return err
		}
	}
	msg := Notification{Method: method, Params: raw}
	envelope := struct {
		JSONRPC string `json:"jsonrpc"`
		Notification
	}{JSONRPC: "2.0", Notification: msg}
	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}

	c.mu.RLock()
	transport := c.transport
	c.mu.RUnlock()

	switch t := transport.(type) {
	case *StdioTransport:
		c.writeMu.Lock()
		_, err := t.stdin.Write(append(data, '\n'))
		c.writeMu.Unlock()
		return err
	case *HTTPTransport:
		req, rerr := http.NewRequestWithContext(ctx, "POST", t.endpoint, strings.NewReader(string(data)))
		if rerr != nil {
			return c.redactError(rerr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
		resp, err := t.do(req)
		if err != nil {
			return c.redactError(err)
		}
		// Notifications get 202 Accepted with no body; drain + close.
		_ = resp.Body.Close()
		return nil
	}
	return nil
}

// handleServerRequest fulfills a server→client JSON-RPC request (a
// message carrying BOTH an id and a method). The two the spec defines
// for our advertised capabilities are roots/list and
// sampling/createMessage; ping is answered too. Anything else gets a
// method-not-found error. The response is written back on the transport.
//
// Without this, server→client requests fell through the response/
// notification split (they have an id, so the dispatcher looked them up
// in c.pending, missed, and dropped them) — leaving the server hung
// waiting for a reply.
func (c *Client) handleServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "roots/list":
		c.mu.RLock()
		roots := append([]Root(nil), c.roots...)
		c.mu.RUnlock()
		c.writeServerResult(id, map[string]any{"roots": roots})
	case "ping":
		c.writeServerResult(id, map[string]any{})
	case "sampling/createMessage":
		if c.sampler == nil {
			c.writeServerError(id, -32601, "client does not support sampling")
			return
		}
		ctx, cancel := context.WithTimeout(c.ctx, RequestTimeout())
		defer cancel()
		// Sampling params are model input just like tool/resource text. A
		// compromised server must not smuggle its configured credential into
		// the host model by echoing it in a sampling request.
		safeParams, err := c.redactJSONRawStrings(params)
		if err != nil {
			c.writeServerError(id, -32602, "invalid sampling parameters")
			return
		}
		res, err := c.sampler(ctx, safeParams)
		if err != nil {
			// Provider errors may include model input or credentials. The server
			// only needs a stable classification, not the raw host error.
			c.writeServerError(id, -32603, "sampling failed")
			return
		}
		c.writeServerRawResult(id, res)
	default:
		c.writeServerError(id, -32601, "method not found: "+method)
	}
}

func (c *Client) dispatchServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	if c.serverRequestSlots == nil {
		c.writeServerError(id, -32000, "server request capacity unavailable")
		return
	}
	select {
	case c.serverRequestSlots <- struct{}{}:
		go func() {
			defer func() { <-c.serverRequestSlots }()
			c.handleServerRequest(id, method, params)
		}()
	default:
		c.writeServerError(id, -32000, "too many concurrent server requests")
	}
}

// writeServerResult marshals result and writes a JSON-RPC response with
// the given id back to the server.
func (c *Client) writeServerResult(id json.RawMessage, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		c.writeServerError(id, -32603, "marshal result: "+err.Error())
		return
	}
	c.writeServerRawResult(id, raw)
}

func (c *Client) writeServerRawResult(id json.RawMessage, raw json.RawMessage) {
	c.writeServerEnvelope(map[string]any{"jsonrpc": "2.0", "id": id, "result": raw})
}

func (c *Client) writeServerError(id json.RawMessage, code int, msg string) {
	c.writeServerEnvelope(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
}

// writeServerEnvelope frames and sends a server→client response. stdio
// writes to stdin; HTTP POSTs the response back to the endpoint (the
// Streamable-HTTP way a client replies to a server request).
func (c *Client) writeServerEnvelope(env map[string]any) {
	data, err := json.Marshal(env)
	if err != nil {
		return
	}
	c.mu.RLock()
	transport := c.transport
	c.mu.RUnlock()
	switch t := transport.(type) {
	case *StdioTransport:
		c.writeMu.Lock()
		_, _ = t.stdin.Write(append(data, '\n'))
		c.writeMu.Unlock()
	case *HTTPTransport:
		// Bound the reply POST so a hung server can't leak this
		// goroutine + connection until the whole client closes (the
		// transport's http.Client has no Timeout).
		ctx, cancel := context.WithTimeout(c.ctx, RequestTimeout())
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", t.endpoint, strings.NewReader(string(data)))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
		resp, err := t.do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}
}

// isServerRequest reports whether a raw message is a server→client
// request (both id and method present) and extracts the pieces.
func isServerRequest(msg []byte) (id json.RawMessage, method string, params json.RawMessage, ok bool) {
	var probe struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(msg, &probe) != nil {
		return nil, "", nil, false
	}
	// A request has an id + method and NO result/error. Requiring the
	// absence of result/error guards against a non-conformant server that
	// echoes a `method` alongside a response — misrouting that to the
	// request handler would strand the real caller waiting on its reply.
	if len(probe.ID) > 0 && string(probe.ID) != "null" && probe.Method != "" &&
		len(probe.Result) == 0 && len(probe.Error) == 0 {
		return probe.ID, probe.Method, probe.Params, true
	}
	return nil, "", nil, false
}

// readLoop pumps JSON-RPC messages from the stdio transport. On EOF or any
// decode error it cancels c.ctx so blocked send() callers wake up with
// ErrTransportClosed instead of waiting on their per-call ctx to expire.
func (c *Client) readLoop() {
	t, ok := c.transport.(*StdioTransport)
	if !ok {
		return
	}
	defer c.cancel() // wake any sender blocked on <-c.ctx.Done()

	scanner := newMCPMessageScanner(t.stdout)
	for scanner.Scan() {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		msg := scanner.Bytes()
		if len(bytes.TrimSpace(msg)) == 0 {
			continue
		}
		// Server→client request (has BOTH id and method) — answer it.
		// Must be checked before the response branch, which keys only on
		// the presence of an id.
		if id, method, params, ok := isServerRequest(msg); ok {
			c.dispatchServerRequest(id, method, params)
			continue
		}
		// Try response (has id)
		var resp JSONRPCResponse
		if err := json.Unmarshal(msg, &resp); err == nil && resp.ID != nil {
			id := fmt.Sprintf("%v", resp.ID)
			c.mu.Lock()
			ch, ok := c.pending[id]
			c.mu.Unlock()
			if ok {
				select {
				case ch <- &resp:
				default:
				}
			}
			continue
		}
		// Try notification
		var notif Notification
		if err := json.Unmarshal(msg, &notif); err == nil && notif.Method != "" {
			select {
			case c.notifications <- notif:
			default:
			}
		}
	}
}

// ListTools returns all tools available on the MCP server.
//
// As with CallTool, a deadline-less ctx gets RequestTimeout() applied
// so a wedged server doesn't hang `/mcp start` indefinitely.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout())
		defer cancel()
	}
	resp, err := c.send(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		// -32601 method-not-found: the server advertises no tools
		// capability (e.g. a resources-only or prompts-only server).
		// Normalize to "no tools" so the handshake still succeeds — same
		// contract as ListPrompts / ListResources.
		if resp.Error.Code == -32601 {
			return nil, nil
		}
		return nil, c.mcpResponseError(resp.Error)
	}
	var result ListToolsResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, c.redactError(err)
	}
	return c.sanitizeTools(result.Tools), nil
}

func (c *Client) sanitizeTools(tools []Tool) []Tool {
	safeTools := make([]Tool, 0, len(tools))
	for i := range tools {
		tool := tools[i]
		if c.containsSensitiveText(tool.Name) {
			continue
		}
		tool.Description = c.RedactText(tool.Description)
		if tool.InputSchema != nil {
			c.redactJSONStrings(tool.InputSchema)
		}
		safeTools = append(safeTools, tool)
	}
	return safeTools[:len(safeTools):len(safeTools)]
}

// SanitizeToolsForExplicitConfig reapplies the same model-boundary filtering
// as a live tools/list response to untrusted persisted cache metadata. Cache
// files can be edited by the same-user MCP child that received explicit
// credentials, so a matching version/fingerprint is not an authenticity
// guarantee.
func SanitizeToolsForExplicitConfig(tools []Tool, env, headers map[string]string, endpoints ...string) []Tool {
	envBindings := make([]string, 0, len(env))
	for key, value := range env {
		envBindings = append(envBindings, key+"="+value)
	}
	values := configuredSecretEnvValues(envBindings)
	values = append(values, configuredSecretHeaderValues(headers)...)
	for _, endpoint := range endpoints {
		if looksCredentialBearingURL(strings.TrimSpace(endpoint)) {
			values = append(values, credentialURLRedactionValues(endpoint, true)...)
		}
	}
	client := NewClient(context.Background(), &StdioTransport{
		redactValues: normalizeExactRedactionValues(values),
	})
	defer client.cancel()
	return client.sanitizeTools(tools)
}

// Prompt is an MCP-server-advertised prompt template the user can
// invoke through the chat surface (`/mcp__<server>__<prompt>`).
// Mirrors Anthropic's prompts/list response shape.
type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptArgument describes one templated input the server expects when
// `prompts/get` is called for this prompt. Required arguments without
// a value cause a slash command to refuse with a usage hint instead of
// firing an empty server call.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// ListPromptsResult mirrors `prompts/list` JSON response.
type ListPromptsResult struct {
	Prompts []Prompt `json:"prompts"`
}

// PromptMessage is one element of a server-rendered prompt body.
// `Role` is "user" or "assistant"; `Content` is a list of typed parts
// (currently always {type:"text", text:"..."}). Mirrors MCP spec.
type PromptMessage struct {
	Role    string             `json:"role"`
	Content []PromptContentRaw `json:"content"`
}

// PromptContentRaw stays raw so clients that only need the text body
// don't pay JSON-decode cost on schema fields they ignore. Helpers
// (PromptText) extract what we need.
type PromptContentRaw struct {
	Type string          `json:"type"`
	Text string          `json:"text,omitempty"`
	Data json.RawMessage `json:"-"`
}

// GetPromptResult is the `prompts/get` response — typically one user
// message whose Content[0].Text is what the chat surface should send.
type GetPromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// ListPrompts returns the prompts a server advertises. Servers without
// the prompts capability return method-not-found; we surface that as
// (nil, nil) so callers (the runtime registrar at startup) can treat
// "no prompts" identically to "this server doesn't support them".
func (c *Client) ListPrompts(ctx context.Context) ([]Prompt, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout())
		defer cancel()
	}
	resp, err := c.send(ctx, "prompts/list", nil)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		// -32601 == method not found. Normalize to "no prompts".
		if resp.Error.Code == -32601 {
			return nil, nil
		}
		return nil, c.mcpResponseError(resp.Error)
	}
	var result ListPromptsResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, c.redactError(err)
	}
	safePrompts := make([]Prompt, 0, len(result.Prompts))
	for i := range result.Prompts {
		prompt := result.Prompts[i]
		if c.containsSensitiveText(prompt.Name) {
			continue
		}
		prompt.Description = c.RedactText(prompt.Description)
		safeArgs := make([]PromptArgument, 0, len(prompt.Arguments))
		for j := range prompt.Arguments {
			arg := prompt.Arguments[j]
			if c.containsSensitiveText(arg.Name) {
				continue
			}
			arg.Description = c.RedactText(arg.Description)
			safeArgs = append(safeArgs, arg)
		}
		prompt.Arguments = safeArgs[:len(safeArgs):len(safeArgs)]
		safePrompts = append(safePrompts, prompt)
	}
	return safePrompts[:len(safePrompts):len(safePrompts)], nil
}

// Resource is an MCP-server-advertised resource (a file, DB row, API
// response, …). On the wire URI is the server's URI; after ListResources
// returns it is an opaque client-local handle safe to show to the model.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ListResourcesResult mirrors the `resources/list` response.
type ListResourcesResult struct {
	Resources []Resource `json:"resources"`
}

// ResourceContent is one returned chunk from resources/read: either text
// (Text set) or base64 binary (Blob set).
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// ReadResourceResult mirrors the `resources/read` response.
type ReadResourceResult struct {
	Contents []ResourceContent `json:"contents"`
}

// ListResources returns the resources a server advertises. Servers
// without the resources capability return method-not-found (-32601),
// which we normalize to (nil, nil) — same "treat absence uniformly"
// contract as ListPrompts.
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout())
		defer cancel()
	}
	resp, err := c.send(ctx, "resources/list", nil)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		if resp.Error.Code == -32601 {
			return nil, nil
		}
		return nil, c.mcpResponseError(resp.Error)
	}
	var result ListResourcesResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, c.redactError(err)
	}
	return c.prepareResources(result.Resources), nil
}

// prepareResources atomically replaces the model-visible resource catalog.
// The list is truncated before any handle can be evicted, ensuring every
// returned opaque handle remains resolvable at the instant this call returns.
func (c *Client) prepareResources(resources []Resource) []Resource {
	capacity := len(resources)
	if capacity > maxResourceHandles {
		capacity = maxResourceHandles
	}
	safeResources := make([]Resource, 0, capacity)

	c.resourceMu.Lock()
	defer c.resourceMu.Unlock()
	if c.resourceClosed {
		return safeResources[:0:0]
	}
	clear(c.resourceByHandle)
	clear(c.resourceByURI)
	c.resourceOrder = nil
	c.resourceURIBytes = 0
	exactValues := make([]string, 0, capacity*2)

	for i := range resources {
		if len(safeResources) >= maxResourceHandles {
			break
		}
		resource := resources[i]
		originalURI := strings.TrimSpace(resource.URI)
		if originalURI == "" || len(originalURI) > maxResourceURIBytes {
			continue
		}
		if _, exists := c.resourceByURI[originalURI]; !exists &&
			c.resourceURIBytes+len(originalURI) > maxResourceURITotalBytes {
			break
		}
		handle, ok := c.resourceHandleLocked(originalURI)
		if !ok {
			continue
		}
		resource.URI = handle
		safeResources = append(safeResources, resource)
		exactValues = append(exactValues, credentialURLRedactionValues(originalURI, true)...)
	}
	// Treat the accepted catalog as one disclosure boundary. A sibling's
	// metadata can echo another resource's signed URI, so every field must be
	// redacted against every accepted URI/component rather than only its own.
	exactValues = normalizeExactRedactionValues(exactValues)
	redactor := c.newTextRedactor(exactValues...)
	for i := range safeResources {
		safeResources[i].Name = redactor.Redact(safeResources[i].Name)
		safeResources[i].Description = redactor.Redact(safeResources[i].Description)
		safeResources[i].MimeType = redactor.Redact(safeResources[i].MimeType)
	}
	return safeResources[:len(safeResources):len(safeResources)]
}

// ReadResource fetches the contents associated with an opaque handle returned
// by ListResources. Raw server URIs never cross back into model/UI callers.
func (c *Client) ReadResource(ctx context.Context, handle string) (*ReadResourceResult, error) {
	uri, err := c.resolveResourceHandle(strings.TrimSpace(handle))
	if err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout())
		defer cancel()
	}
	resp, err := c.send(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, c.mcpResponseErrorWithExact(resp.Error, credentialURLRedactionValues(uri, true)...)
	}
	var result ReadResourceResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, c.redactError(err)
	}
	exactURIValues := credentialURLRedactionValues(uri, true)
	contentURIs := make([]string, len(result.Contents))
	for i := range result.Contents {
		contentURI := strings.TrimSpace(result.Contents[i].URI)
		contentURIs[i] = contentURI
		if contentURI != "" {
			exactURIValues = append(exactURIValues, credentialURLRedactionValues(contentURI, true)...)
		}
	}
	exactURIValues = normalizeExactRedactionValues(exactURIValues)

	c.resourceMu.Lock()
	for i := range result.Contents {
		contentURI := contentURIs[i]
		if contentURI == "" {
			result.Contents[i].URI = handle
			continue
		}
		contentHandle, ok := c.resourceHandleLockedNoEvict(contentURI)
		if !ok {
			result.Contents[i].URI = handle
			continue
		}
		result.Contents[i].URI = contentHandle
	}
	c.resourceMu.Unlock()
	redactor := c.newTextRedactor(exactURIValues...)
	for i := range result.Contents {
		result.Contents[i].MimeType = redactor.Redact(result.Contents[i].MimeType)
		result.Contents[i].Text = redactor.Redact(result.Contents[i].Text)
		// Blob is base64 binary data. It must remain byte-for-byte unchanged.
	}
	return &result, nil
}

// GetPrompt resolves a prompt template against `args`. Returns the
// server-rendered messages — caller picks the body to send.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (*GetPromptResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout())
		defer cancel()
	}
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	resp, err := c.send(ctx, "prompts/get", params)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, c.mcpResponseError(resp.Error)
	}
	var result GetPromptResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, c.redactError(err)
	}
	result.Description = c.RedactText(result.Description)
	safeMessages := make([]PromptMessage, 0, len(result.Messages))
	for i := range result.Messages {
		message := result.Messages[i]
		if c.containsSensitiveText(message.Role) {
			continue
		}
		safeContent := make([]PromptContentRaw, 0, len(message.Content))
		for j := range message.Content {
			content := message.Content[j]
			if c.containsSensitiveText(content.Type) {
				continue
			}
			content.Text = c.RedactText(content.Text)
			safeContent = append(safeContent, content)
		}
		message.Content = safeContent[:len(safeContent):len(safeContent)]
		safeMessages = append(safeMessages, message)
	}
	result.Messages = safeMessages[:len(safeMessages):len(safeMessages)]
	return &result, nil
}

// CallTool invokes an MCP tool with the given arguments.
//
// If the caller's ctx has no deadline set, ToolTimeout() is applied so
// a misbehaving tool can't hang the agent loop forever. A caller that
// wants the long path explicitly (interactive flows, debug sessions)
// should set its own deadline before calling.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]interface{}) (json.RawMessage, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ToolTimeout())
		defer cancel()
	}
	params := map[string]interface{}{
		"name":      name,
		"arguments": args,
	}
	resp, err := c.send(ctx, "tools/call", params)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, c.mcpResponseError(resp.Error)
	}
	return resp.Result, nil
}

// HTTPTransport implements the MCP Streamable HTTP transport. Single
// endpoint for both request-response (POST) and server-initiated
// notifications (long-poll GET with Accept: text/event-stream).
type HTTPTransport struct {
	endpoint string
	client   *http.Client
	headers  map[string]string

	// cancelGET is set by httpNotificationLoop when it opens the GET
	// SSE channel. Calling it (via Close → cancel ctx) closes the
	// notification stream cleanly.
	cancelMu  sync.Mutex
	cancelGET context.CancelFunc
	closed    bool
}

func newMCPHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: checkMCPSameOriginRedirect}
}

func checkMCPSameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errHTTPTooManyRedirects
	}
	if len(via) == 0 || sameMCPHTTPOrigin(via[0].URL, req.URL) {
		return nil
	}
	return errHTTPCrossOriginRedirect
}

func sameMCPHTTPOrigin(left, right *url.URL) bool {
	if left == nil || right == nil ||
		!strings.EqualFold(left.Scheme, right.Scheme) ||
		!strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	return mcpHTTPPort(left) == mcpHTTPPort(right)
}

func mcpHTTPPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func (t *HTTPTransport) do(req *http.Request) (*http.Response, error) {
	resp, err := t.client.Do(req)
	if errors.Is(err, errHTTPCrossOriginRedirect) || errors.Is(err, errHTTPTooManyRedirects) {
		// http.Client wraps redirect-policy failures in url.Error and includes
		// the untrusted Location. Returning only the stable sentinel prevents
		// credential-bearing redirect URLs from reaching logs or the UI.
		if errors.Is(err, errHTTPCrossOriginRedirect) {
			return nil, errHTTPCrossOriginRedirect
		}
		return nil, errHTTPTooManyRedirects
	}
	return resp, err
}

func (t *HTTPTransport) Close() error {
	t.cancelMu.Lock()
	if t.closed {
		t.cancelMu.Unlock()
		return nil
	}
	t.closed = true
	cancel := t.cancelGET
	t.cancelGET = nil
	t.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (t *HTTPTransport) setCancelGET(cancel context.CancelFunc) {
	t.cancelMu.Lock()
	if t.closed {
		t.cancelMu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	previous := t.cancelGET
	t.cancelGET = cancel
	t.cancelMu.Unlock()
	if previous != nil {
		previous()
	}
}

// httpNotificationLoop opens a GET on the endpoint and reads SSE
// frames so the server can push notifications. It only runs for
// HTTPTransport-backed clients; stdio clients use readLoop instead.
//
// Failures here are non-fatal — many MCP servers don't support the GET
// notification channel, in which case we log once and move on. The
// request-response path (POST) keeps working either way.
func (c *Client) httpNotificationLoop() {
	ht, ok := c.transport.(*HTTPTransport)
	if !ok {
		return
	}
	getCtx, cancel := context.WithCancel(c.ctx)
	ht.setCancelGET(cancel)

	req, err := http.NewRequestWithContext(getCtx, "GET", ht.endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range ht.headers {
		req.Header.Set(k, v)
	}
	resp, err := ht.do(req)
	if err != nil {
		return // server doesn't support GET-SSE — silent skip
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return
	}
	_ = c.parseSSE(resp.Body)
}

// parseSSE reads `data: <json>` lines and dispatches each into the
// existing pending/notifications channels, same as readLoop for stdio.
func (c *Client) parseSSE(r io.Reader) error {
	scanner := newMCPMessageScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		c.dispatchJSONRPC([]byte(payload))
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("MCP SSE frame exceeds limit or is unreadable: %w", err)
	}
	return nil
}

// dispatchJSONRPC routes one raw JSON-RPC message into either the
// pending response map (when it has an id) or the notifications
// channel (no id). Shared between stdio and HTTP+SSE paths so the
// per-transport code only needs to forward bytes.
func (c *Client) dispatchJSONRPC(data []byte) {
	// Server→client request (id AND method) — answer it before the
	// response/notification split below.
	if id, method, params, ok := isServerRequest(data); ok {
		c.dispatchServerRequest(id, method, params)
		return
	}
	var probe struct {
		ID interface{} `json:"id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return
	}
	if probe.ID != nil {
		var resp JSONRPCResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return
		}
		id := fmt.Sprintf("%v", resp.ID)
		c.mu.RLock()
		ch, ok := c.pending[id]
		c.mu.RUnlock()
		if ok {
			select {
			case ch <- &resp:
			default:
			}
		}
		return
	}
	var notif Notification
	if err := json.Unmarshal(data, &notif); err == nil && notif.Method != "" {
		select {
		case c.notifications <- notif:
		default:
		}
	}
}
