package main

// mcp_prompts_bind.go — Phase D #40 binder. Lives in cmd/metis/ so it
// can import both `internal/slash` (for the registry) and
// `internal/runtime` (for the handle type) without forming the import
// cycle that would result from putting it in either of them.

import (
	"context"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/slash"
)

// registerMCPPromptsAsSlash binds each handle as a slash command whose
// handler returns SignalCustomPrompt — same contract user-authored
// .md commands use. The chat surface gets the rendered prompt body and
// pushes it as the next user message.
//
// Returns the registered slash names so the caller can log "loaded N
// MCP prompts" at startup if it wants to.
func registerMCPPromptsAsSlash(r *slash.Registry, handles []mcp.PromptHandle) []string {
	if r == nil {
		return nil
	}
	var names []string
	for i := range handles {
		h := handles[i] // capture
		desc := strings.TrimSpace(h.Description)
		if desc == "" {
			desc = "MCP prompt from " + h.ServerName
		}
		r.Register(slash.Cmd{
			Name:         h.SlashName,
			Description:  desc,
			ArgumentHint: strings.Join(h.Arguments, " "),
			Source:       "mcp:" + h.ServerName,
			Category:     "mcp",
			Handler: func(args string) (string, slash.Signal) {
				argv := strings.Fields(args)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				body, err := h.Resolve(ctx, argv)
				if err != nil {
					return "(mcp prompt " + h.SlashName + ": " + err.Error() + ")", slash.SignalNone
				}
				return body, slash.SignalCustomPrompt
			},
		})
		names = append(names, h.SlashName)
	}
	return names
}
