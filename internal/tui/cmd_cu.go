package tui

// /cu — one-line computer-use (metis-cu) management. Thin wrapper
// around /mcp that hardcodes the name ("computer-use"), the binary
// ("metis-cu"), and an in-session LaunchMCPServer so the tools are
// visible on the next turn instead of waiting for a metis restart.
//
// Why a dedicated command instead of just "/mcp add computer-use metis-cu":
//   - typo-proof: name + command are fixed by spec, eliminating the
//     "I added it as `cu` but Anthropic's prompts expect `computer-use`"
//     class of mistake.
//   - one-step: `/mcp add` requires a follow-up `/mcp start` to
//     hot-load tools; `/cu enable` does both.
//   - sanity check: warns when metis-cu binary isn't in PATH so users
//     learn before the first tool call fails.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

// cuServerName is the MCP-side name we hardcode. It mirrors Anthropic's
// `mcp__computer-use__*` namespace exactly so prompts and traces written
// for Claude Code's built-in computer-use server are interoperable.
const cuServerName = "computer-use"

// cuBinaryName is the binary metis-cu installs as. `make install` from
// the metis-cu repo writes this to ~/go/bin/metis-cu (and
// ~/.local/bin/metis-cu via copy).
const cuBinaryName = "metis-cu"

func cmdCU(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return cuStatus()
	}
	parts := strings.Fields(args)
	switch parts[0] {
	case "enable", "on":
		return cuEnable(r)
	case "disable", "off":
		return cuDisable()
	case "status":
		return cuStatus()
	}
	return "cu: unknown '" + parts[0] + "'. usage: cu enable | cu disable | cu status"
}

// cuEnable writes the metis-cu entry to mcp.toml, then hot-loads the
// server into the live registry so tools are visible immediately.
// Idempotent: re-running while enabled produces a "(replaced...)" line
// rather than an error so the user can re-enable without first
// disabling.
func cuEnable(r *REPL) string {
	binPath, lookErr := exec.LookPath(cuBinaryName)
	if lookErr != nil {
		return fmt.Sprintf("cu: %s not in PATH — install via: cd metis-cu && make install\n  (or: go install github.com/Ricardo-M-L/metis-cu@latest)", cuBinaryName)
	}

	reg, err := mcp.Load()
	if err != nil {
		return "cu: load mcp.toml: " + err.Error()
	}
	// Use the dedicated SetReservedComputerUseServer rather than
	// AddMCPServer — AddMCPServer refuses the reserved name (it's the
	// guardrail for `/mcp add computer-use ...`). /cu owns this slot.
	existed := mcp.SetReservedComputerUseServer(reg)
	// SetReservedComputerUseServer leaves Disabled at its prior value
	// when replacing; force enabled so re-running `/cu enable` after
	// `/cu disable` actually flips it back on.
	if e := mcp.FindServer(reg, cuServerName); e != nil {
		e.Disabled = false
	}
	if err := mcp.Save(reg); err != nil {
		return "cu: save: " + err.Error()
	}

	// Hot-load into the live registry so tools are usable this turn.
	// Use the same 30s timeout as /mcp start; metis-cu spawns
	// near-instantly so this is a generous upper bound.
	base := context.Background()
	if r != nil && r.ctx != nil {
		base = r.ctx
	}
	ctx, cancel := context.WithTimeout(base, 30*time.Second)
	defer cancel()
	if r != nil && r.Loop != nil && r.Loop.Registry != nil {
		staged := tools.NewRegistry()
		srv, err := launchMCPServerWithLifecycle(ctx, base, func(liveCtx context.Context) (*mcptools.Server, error) {
			return mcp.LaunchServerWithSandbox(liveCtx, reg, cuServerName, staged, r.sandbox)
		})
		if err != nil {
			// Persistence already succeeded — the next metis start
			// will spawn it. Surface the live-load error so the user
			// knows tools won't appear until restart.
			return fmt.Sprintf("cu: enabled in mcp.toml but live-load failed: %v\n  (tools will appear on next metis start)", err)
		}
		toolCount, ownsServer := adoptOrPublishMCPLoginLaunch(
			r.Loop.Registry, cuServerName,
			mcpLoginLaunch{server: srv, tools: staged.All()}, r.AdoptMCPServer,
		)
		if ownsServer {
			r.mcpLoginServers = append(r.mcpLoginServers, srv)
		}
		verb := "enabled"
		if existed {
			verb = "re-enabled"
		}
		return fmt.Sprintf("cu: %s — %s (%d tools); binary=%s",
			verb, cuServerName, toolCount, binPath)
	}
	// REPL not fully wired (test harness or boot path); persistence
	// only — restart picks it up.
	verb := "enabled"
	if existed {
		verb = "re-enabled"
	}
	return fmt.Sprintf("cu: %s in mcp.toml; binary=%s\n  (restart metis to load tools)",
		verb, binPath)
}

func cuDisable() string {
	reg, err := mcp.Load()
	if err != nil {
		return "cu: load mcp.toml: " + err.Error()
	}
	if !mcp.RemoveServer(reg, cuServerName) {
		return "cu: not enabled (no `" + cuServerName + "` entry in mcp.toml)"
	}
	if err := mcp.Save(reg); err != nil {
		return "cu: save: " + err.Error()
	}
	return "cu: disabled — tools remain in this session until restart"
}

// cuStatus returns a one-line state summary plus the binary path when
// resolvable. Designed for the bare `/cu` (no args) form where the user
// is asking "what's the situation?" without committing to a change.
func cuStatus() string {
	binPath, lookErr := exec.LookPath(cuBinaryName)
	binState := binPath
	if lookErr != nil {
		binState = "not found in PATH"
	}
	reg, err := mcp.Load()
	if err != nil {
		return "cu: load mcp.toml: " + err.Error()
	}
	entry := mcp.FindServer(reg, cuServerName)
	switch {
	case entry == nil:
		return fmt.Sprintf("cu: not enabled. binary: %s\n  run `/cu enable` to register + hot-load", binState)
	case entry.Disabled:
		return fmt.Sprintf("cu: disabled (in mcp.toml). binary: %s", binState)
	default:
		return fmt.Sprintf("cu: enabled. binary: %s; mcp.toml entry uses command=%q",
			binState, entry.Command)
	}
}
