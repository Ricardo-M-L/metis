package runtime

// ide.go — discovery of a running IDE's MCP server via lockfiles, the
// metis side of the CC-style IDE integration. A VS Code (or other)
// extension that hosts an MCP server writes a lockfile to ~/.metis/ide/
// describing how to reach it; metis scans that directory on startup and,
// when it finds a live server whose workspace contains the current
// directory, connects to it as an MCP client. The server's tools
// (openDiff / getDiagnostics / getCurrentSelection) then appear to the
// model like any other MCP tool.
//
// Transport note: CC's IDE MCP uses WebSocket. metis reuses its existing
// Streamable-HTTP/SSE MCP client instead (both are valid MCP transports),
// so the lockfile carries an HTTP port and metis needs no new ws stack.
//
// This file is pure discovery — no MCP import — so it stays unit-testable
// in isolation. cmd/metis wires the discovered endpoint into
// mcp.NewHTTPServer.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// IDELock is the on-disk description an IDE extension writes so metis can
// find and authenticate to its MCP server. Filename convention:
// ~/.metis/ide/<port>.lock.
type IDELock struct {
	PID              int      `json:"pid"`
	Port             int      `json:"port"`
	Transport        string   `json:"transport"` // "http" (Streamable HTTP/SSE)
	IDEName          string   `json:"ideName"`   // "VS Code", "Cursor", …
	WorkspaceFolders []string `json:"workspaceFolders"`
	AuthToken        string   `json:"authToken"`
}

// IDEDir is where IDE extensions drop their lockfiles.
func IDEDir() string {
	return filepath.Join(config.Home(), "ide")
}

// Endpoint is the Streamable-HTTP MCP URL for this server.
func (l *IDELock) Endpoint() string {
	return "http://127.0.0.1:" + strconv.Itoa(l.Port) + "/mcp"
}

// AuthHeaders returns the headers metis should attach when connecting,
// or nil when the server declared no token.
func (l *IDELock) AuthHeaders() map[string]string {
	if l.AuthToken == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + l.AuthToken}
}

// DiscoverIDE scans ~/.metis/ide for live IDE MCP servers and returns the
// best match for cwd: a server whose workspaceFolders contains cwd wins;
// otherwise, if exactly one live server exists, it's used; otherwise no
// match. Dead servers (lockfile present but PID gone) are ignored and
// their stale lockfiles swept.
func DiscoverIDE(cwd string) (*IDELock, bool) {
	locks := liveIDELocks()
	if len(locks) == 0 {
		return nil, false
	}
	cwd = filepath.Clean(cwd)
	// Prefer the server whose workspace contains cwd. Among multiple
	// matches, the deepest (longest) workspace path wins — it's the most
	// specific project for this directory.
	var best *IDELock
	var bestLen int
	for _, l := range locks {
		for _, wf := range l.WorkspaceFolders {
			wf = filepath.Clean(wf)
			if cwd == wf || strings.HasPrefix(cwd, wf+string(filepath.Separator)) {
				if best == nil || len(wf) > bestLen {
					best = l
					bestLen = len(wf)
				}
			}
		}
	}
	if best != nil {
		return best, true
	}
	// No workspace match. Fall back to the single-server case so a metis
	// launched outside the workspace tree still attaches to the obvious
	// IDE. Ambiguous (2+) → don't guess.
	if len(locks) == 1 {
		return locks[0], true
	}
	return nil, false
}

// liveIDELocks reads every lockfile, drops the ones whose process is
// dead (sweeping the stale file), and returns the survivors sorted by
// port for deterministic ordering.
func liveIDELocks() []*IDELock {
	dir := IDEDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []*IDELock
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		l, err := readIDELock(p)
		if err != nil {
			continue
		}
		if !processAlive(l.PID) {
			_ = os.Remove(p) // sweep stale lockfile
			continue
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func readIDELock(path string) (*IDELock, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l IDELock
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// processAlive reports whether pid names a running process. Uses signal 0
// (the POSIX "is it there" probe) — no signal is delivered. A zero/neg
// pid is treated as dead.
//
// EPERM means the process EXISTS but is owned by another user (e.g. the
// IDE runs under a different uid than metis) — that's alive, not dead.
// Only ESRCH ("no such process") counts as dead. Getting this wrong
// would make liveIDELocks sweep a perfectly valid lockfile.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix FindProcess always succeeds; Signal(0) is the real check.
	err = proc.Signal(syscall.Signal(0))
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}
