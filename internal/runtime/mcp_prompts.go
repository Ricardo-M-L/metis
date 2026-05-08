package runtime

// mcp_prompts.go — Phase D #40. Walks every launched MCP server,
// asks it for its prompts (prompts/list, optional capability), and
// builds a list of `mcp__<server>__<prompt>` slash commands the
// caller registers with the slash.Registry.
//
// Why a registrar separate from launch:
//   - launch sequencing already loops over LaunchAllMCP; adding
//     prompts/list there would slow startup for the (today common)
//     case of zero-prompts servers.
//   - the slash registry is owned by the chat surface, not the
//     runtime — making prompts visible needs a dedicated handoff.
//
// The slash command body delegates to GetPrompt at invocation time
// (not at registration). That means the LLM-facing argument substitution
// happens server-side, mirroring Crush's design and matching what
// claude-code does internally.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/mcp"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

// MCPPromptHandle is the value the chat surface acts on when the user
// types `/mcp__<server>__<prompt> arg1 arg2 ...`. It carries enough
// state to call GetPrompt without re-walking the registry. The chat
// surface (or any other dispatcher) renders Description in `/help`.
type MCPPromptHandle struct {
	SlashName   string // e.g. "mcp__github__pr_summary"
	ServerName  string // raw server logical name
	PromptName  string // server-side name
	Description string
	Arguments   []string // arg names in declared order; * suffix means optional
	server      *mcptools.Server
}

// Resolve calls prompts/get on the server and returns the rendered
// body the user message should carry. Maps positional argv into the
// argument list declared by the server.
func (h *MCPPromptHandle) Resolve(ctx context.Context, argv []string) (string, error) {
	if h.server == nil {
		return "", fmt.Errorf("mcp prompt %q: server reference lost", h.SlashName)
	}
	args := map[string]string{}
	for i, name := range h.Arguments {
		if i >= len(argv) {
			break
		}
		// `*` suffix marks optional names — strip it before key lookup.
		key := strings.TrimSuffix(name, "*")
		args[key] = argv[i]
	}
	res, err := h.server.GetPrompt(ctx, h.PromptName, args)
	if err != nil {
		return "", err
	}
	if res == nil || len(res.Messages) == 0 {
		return "", fmt.Errorf("mcp prompt %q: server returned no messages", h.SlashName)
	}
	// Concat every text part of every message in declared order. Most
	// servers return a single user message; we don't try to dispatch by
	// role (the chat surface flattens this into one user prompt anyway).
	var b strings.Builder
	for _, m := range res.Messages {
		for _, c := range m.Content {
			if c.Type == "text" && c.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(c.Text)
			}
		}
	}
	return strings.TrimSpace(b.String()), nil
}

// CollectMCPPrompts probes each server for its prompts and returns
// the discovered handles. Per-server failure (missing capability,
// transient error) only logs to stderr — we never let one bad server
// block the rest. Caller passes the live []*Server slice from
// LaunchAllMCP / LaunchMCPServer.
func CollectMCPPrompts(ctx context.Context, servers []*mcptools.Server) []MCPPromptHandle {
	var out []MCPPromptHandle
	for _, srv := range servers {
		if srv == nil {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		prompts, err := srv.ListPrompts(probeCtx)
		cancel()
		if err != nil {
			// Most common cause: server doesn't implement prompts/list,
			// or the transport hiccupped. Soft-fail — skill is "list of
			// prompts is the union across servers; one bad server just
			// drops out".
			continue
		}
		for _, p := range prompts {
			out = append(out, MCPPromptHandle{
				SlashName:   "mcp__" + srv.Name() + "__" + p.Name,
				ServerName:  srv.Name(),
				PromptName:  p.Name,
				Description: p.Description,
				Arguments:   argNames(p.Arguments),
				server:      srv,
			})
		}
	}
	return out
}

// PromptDescription returns the help-line text for a slash registration.
// Pulled out of the registrar so the (cmd/metis-side) binder doesn't
// need to duplicate the formatting rule.
func (h *MCPPromptHandle) PromptDescription() string {
	desc := h.Description
	if desc == "" {
		desc = "(MCP prompt from " + h.ServerName + ")"
	}
	if len(h.Arguments) > 0 {
		desc += " · args: " + strings.Join(h.Arguments, ", ")
	}
	return desc
}

// argNames flattens PromptArgument list into "name" or "name*" (the
// asterisk marks optional). Stable ordering: the server returns them
// in declaration order; we preserve that for the slash help string.
func argNames(args []mcp.PromptArgument) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a.Required {
			out = append(out, a.Name)
		} else {
			out = append(out, a.Name+"*")
		}
	}
	return out
}
