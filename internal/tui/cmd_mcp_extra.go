package tui

// cmd_mcp_extra.go — extra /mcp subcommands beyond list/add/remove/start.
//
// Phase A of the Claude Code parity plan. Each helper is a method on *REPL
// so cmdMCP's switch can dispatch into them without dragging extra state
// through the call chain. Filenames mirror cmd_cu.go's split — keep the
// huge commands.go free of per-feature plumbing.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// handleMCPEnable / handleMCPDisable flip the Disabled field on the
// named server and persist mcp.toml. Idempotent: enabling an already-
// enabled server returns a clear "(no change)" message rather than
// rewriting the file with identical content.
func (r *REPL) handleMCPEnable(name string) string {
	return r.setMCPState(name, false)
}

func (r *REPL) handleMCPDisable(name string) string {
	return r.setMCPState(name, true)
}

func (r *REPL) setMCPState(name string, disabled bool) string {
	reg, err := mcp.Load()
	if err != nil {
		return "mcp: " + err.Error()
	}
	found, prior := mcp.SetDisabled(reg, name, disabled)
	if !found {
		return "(no MCP server named: " + name + ")"
	}
	if prior == disabled {
		state := "enabled"
		if disabled {
			state = "disabled"
		}
		return "(MCP server " + name + " already " + state + ")"
	}
	if err := mcp.Save(reg); err != nil {
		return "mcp: save: " + err.Error()
	}
	if disabled {
		return "(disabled MCP server: " + name + " — restart metis to remove its tools from the registry)"
	}
	return "(enabled MCP server: " + name + " — `mcp start " + name + "` to spawn now or restart metis)"
}

// handleMCPEdit opens ~/.metis/mcp.toml in $EDITOR. The whole file is
// the unit of edit — there's no per-server fragment to splice — so we
// jump the user there and let them tweak. After the editor returns we
// re-load the file to surface obvious decode errors before they bite at
// next launch. Honors $VISUAL → $EDITOR → vi as the resolver chain.
func (r *REPL) handleMCPEdit(name string) string {
	path := mcp.Path()
	// Pre-flight: confirm the named server is real BEFORE launching the
	// editor. Saves a round-trip when the user typo'd the name.
	reg, err := mcp.Load()
	if err != nil {
		return "mcp: " + err.Error()
	}
	if name != "" && mcp.FindServer(reg, name) == nil {
		return "(no MCP server named: " + name + " — `/mcp list` to see what's registered)"
	}
	editor := pickEditor()
	cmd := exec.Command(editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "mcp edit: " + err.Error()
	}
	// Re-load to validate the user's edit. Don't re-launch live servers
	// — that would surprise mid-turn. /mcp reload is the explicit path
	// for that.
	if _, err := mcp.Load(); err != nil {
		return "mcp edit: file saved but parse failed — fix it before `/mcp reload`:\n  " + err.Error()
	}
	return "(mcp.toml saved · run `/mcp reload` to apply)"
}

// handleMCPTest performs a one-shot connect → ListTools → Close against
// the named server. Useful for debugging an `npx mcp-server-foo` that
// won't start: the user gets a real error instead of a silent missing-
// tools state. We don't graft the tools onto r.Loop.Registry — this is
// a probe, not a launch.
func (r *REPL) handleMCPTest(name string) string {
	reg, err := mcp.Load()
	if err != nil {
		return "mcp: " + err.Error()
	}
	entry := mcp.FindServer(reg, name)
	if entry == nil {
		return "(no MCP server named: " + name + ")"
	}
	if entry.Disabled {
		return "(MCP server " + name + " is disabled — `/mcp enable " + name + "` first)"
	}
	// 15s probe timeout — long enough for `npx` to fetch a package on
	// a slow link, short enough that a misconfigured URL doesn't hang
	// the chat surface.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Use a throwaway tools.Registry; mcp.LaunchServer registers
	// onto whatever we pass, but we want the count, not the side effect.
	// The simpler `srv, err := mcptools.NewServer(...)` would skip env
	// expansion + the URL/stdio branching, so route through LaunchMCPServer
	// with a discard registry.
	probe := tools.NewRegistry()
	srv, err := mcp.LaunchServerWithSandbox(ctx, reg, name, probe, r.sandbox)
	if err != nil {
		return "mcp test: " + err.Error()
	}
	defer srv.Close()
	tools := srv.Tools()
	transport := "stdio"
	if entry.URL != "" {
		transport = "http"
	}
	return fmt.Sprintf("(mcp test %s: ok · transport=%s · %d tools listed)", name, transport, len(tools))
}

// handleMCPLogs surfaces the captured stderr for an MCP server. Today
// the launcher doesn't tee subprocess stderr to disk — a sub-process
// just inherits the metis stderr, so this command tells the user where
// to look (the binary's own logs) and points at the upstream issue.
//
// Phase A surfaces the path; Phase E (daemon work) will land actual
// stderr capture into ~/.metis/mcp-logs/<name>.log. Until then this
// command is a structured dead-end rather than a confidently-empty
// answer.
func (r *REPL) handleMCPLogs(name string) string {
	reg, err := mcp.Load()
	if err != nil {
		return "mcp: " + err.Error()
	}
	if entry := mcp.FindServer(reg, name); entry == nil {
		return "(no MCP server named: " + name + ")"
	}
	logDir := filepath.Join(filepath.Dir(mcp.Path()), "mcp-logs")
	logPath := filepath.Join(logDir, name+".log")
	if data, err := os.ReadFile(logPath); err == nil {
		// Tail the last ~80 lines so a long-running server doesn't
		// fill the chat with a megabyte of stderr.
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		const tail = 80
		if len(lines) > tail {
			lines = lines[len(lines)-tail:]
		}
		return fmt.Sprintf("(mcp logs %s · last %d lines from %s)\n%s",
			name, len(lines), logPath, strings.Join(lines, "\n"))
	}
	return "(no captured logs at " + logPath + " — stderr currently inherits metis's tty;\n" +
		"  log capture lands with the daemon work in Phase E)"
}

// handleMCPReload re-reads mcp.toml and surfaces any parse error. It
// does NOT shut down already-running servers — that's a process-level
// concern that needs the daemon work to do safely (live tool unregister
// hasn't been wired through tools.Registry yet). The user's typical
// workflow after /mcp edit is: edit → reload (catch errors) → restart
// metis (apply). Reload is honest about that today; once the daemon is
// in place it'll grow real hot-swap semantics.
func (r *REPL) handleMCPReload() string {
	reg, err := mcp.Load()
	if err != nil {
		return "mcp reload: " + err.Error()
	}
	enabled := 0
	disabled := 0
	for _, s := range reg.Servers {
		if s.Disabled {
			disabled++
		} else {
			enabled++
		}
	}
	return fmt.Sprintf("(mcp reload · ok · %d enabled, %d disabled · restart metis to apply)",
		enabled, disabled)
}

// pickEditor returns the user's preferred editor binary, in claude-
// code's resolution order: $VISUAL, $EDITOR, then `vi` as the universal
// fallback. Pulled out so /mcp edit, /skills edit, and any future "open
// in editor" surface share one rule.
func pickEditor() string {
	if v := os.Getenv("VISUAL"); v != "" {
		return v
	}
	if v := os.Getenv("EDITOR"); v != "" {
		return v
	}
	return "vi"
}
