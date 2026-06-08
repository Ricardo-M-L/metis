package main

// ide_connect.go — connect metis to a running IDE's MCP server (the
// metis side of the CC-style IDE integration). A VS Code/Cursor
// extension hosting an MCP server writes a lockfile under ~/.metis/ide/;
// runtime.DiscoverIDE finds the one matching the current workspace, and
// here we connect to it over the existing Streamable-HTTP MCP client and
// register its tools (openDiff / getDiagnostics / getCurrentSelection)
// as `mcp__ide__*` so the model can use them like any other MCP tool.
//
// Best-effort: any failure (no IDE running, handshake error) is logged
// and ignored — metis works fine without an IDE attached.

import (
	"context"
	"fmt"
	"os"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

// connectIDE discovers and attaches to a running IDE MCP server. It
// returns the connected server (for Cleanup tracking) or nil when no IDE
// is found or the connection fails. logw receives a one-line status note;
// pass nil to silence it.
//
// logw is a concrete *os.File (not io.Writer) on purpose: callers pass
// the debug log handle, which is a nil *os.File when debug logging is off.
// As an io.Writer that nil would become a non-nil interface and slip past
// a `!= nil` check (then silently no-op every write); a concrete-pointer
// nil check is honest.
func connectIDE(ctx context.Context, reg *tools.Registry, logw *os.File) *mcptools.Server {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	lock, ok := rtpkg.DiscoverIDE(cwd)
	if !ok {
		return nil
	}
	srv, err := mcptools.NewHTTPServer(ctx, "ide", lock.Endpoint(), lock.AuthHeaders())
	if err != nil {
		if logw != nil {
			fmt.Fprintf(logw, "metis: IDE MCP connect (%s @ %s): %v\n", lock.IDEName, lock.Endpoint(), err)
		}
		return nil
	}
	for _, t := range srv.Tools() {
		reg.Register(t)
	}
	if logw != nil {
		fmt.Fprintf(logw, "metis: attached to %s IDE MCP at %s (%d tools)\n", lock.IDEName, lock.Endpoint(), len(srv.Tools()))
	}
	return srv
}
