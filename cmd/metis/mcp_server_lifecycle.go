package main

import (
	"context"
	"time"

	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

func mcpToolsForServer(registry *tools.Registry, serverName string) []tools.Tool {
	if registry == nil {
		return nil
	}
	prefix := "mcp__" + serverName + "__"
	out := make([]tools.Tool, 0)
	for _, candidate := range registry.All() {
		if len(candidate.Name()) >= len(prefix) && candidate.Name()[:len(prefix)] == prefix {
			out = append(out, candidate)
		}
	}
	return out
}

// adoptMCPServer makes the live server list and its tool namespace one
// ownership transaction. Explicit reauthentication has priority over the
// asynchronous startup launcher; a late startup result is closed and cannot
// republish stale tools or a sticky-failed client.
func (r *runtime) adoptMCPServer(server *mcptools.Server, discovered []tools.Tool, explicit bool) bool {
	if r == nil || server == nil {
		return false
	}
	name := server.Name()
	r.mcpServersMu.Lock()
	if r.mcpClosing {
		r.mcpServersMu.Unlock()
		_ = server.Close()
		// true means the ownership callback consumed the server. Returning
		// false would tell the TUI to fallback-publish this already-closed late
		// result into the live registry.
		return true
	}
	if r.mcpExplicitServers == nil {
		r.mcpExplicitServers = make(map[string]struct{})
	}
	if !explicit {
		if _, replaced := r.mcpExplicitServers[name]; replaced {
			r.mcpServersMu.Unlock()
			_ = server.Close()
			return false
		}
	} else {
		r.mcpExplicitServers[name] = struct{}{}
	}

	prior := make([]*mcptools.Server, 0, 1)
	kept := r.mcpServers[:0]
	for _, current := range r.mcpServers {
		if current != nil && current.Name() == name {
			if current != server {
				prior = append(prior, current)
			}
			continue
		}
		kept = append(kept, current)
	}
	r.mcpServers = append(kept, server)
	if r.registry != nil {
		r.registry.ReplacePrefix("mcp__"+name+"__", discovered)
	}
	r.mcpServersMu.Unlock()

	for _, old := range prior {
		_ = old.Close()
	}
	return true
}

func (r *runtime) registerLiveMCPPrompts(ctx context.Context, registry *slash.Registry, server *mcptools.Server) {
	if r == nil || registry == nil || server == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	handles := mcp.CollectPrompts(probeCtx, []*mcptools.Server{server})
	cancel()

	// Discovery is intentionally outside the lifecycle mutex. Re-check the
	// exact pointer before publishing so a slower prior reconnect cannot remove
	// or overwrite prompts from a newer explicit login.
	r.mcpServersMu.Lock()
	current := false
	if !r.mcpClosing {
		for _, candidate := range r.mcpServers {
			if candidate == server {
				current = true
				break
			}
		}
	}
	if !current {
		r.mcpServersMu.Unlock()
		return
	}
	registry.RemoveSource("mcp:" + server.Name())
	_ = registerMCPPromptsAsSlash(registry, handles)
	r.mcpServersMu.Unlock()
}

// startMCPPromptTask establishes a shutdown-safe Add/Wait boundary. Add is
// serialized with mcpClosing, so Cleanup can mark closing and then Wait without
// racing a new prompt goroutine into the WaitGroup.
func (r *runtime) startMCPPromptTask(run func(context.Context)) {
	if r == nil || run == nil {
		return
	}
	r.mcpServersMu.Lock()
	if r.mcpClosing {
		r.mcpServersMu.Unlock()
		return
	}
	ctx := r.mcpPromptCtx
	if ctx == nil {
		ctx = context.Background()
	}
	r.mcpPromptWG.Add(1)
	r.mcpServersMu.Unlock()
	go func() {
		defer r.mcpPromptWG.Done()
		run(ctx)
	}()
}

func (r *runtime) scheduleLiveMCPPrompts(registry *slash.Registry, server *mcptools.Server) {
	if registry == nil || server == nil {
		return
	}
	r.startMCPPromptTask(func(ctx context.Context) {
		r.registerLiveMCPPrompts(ctx, registry, server)
	})
}

// scheduleMCPPromptsAfterLaunch closes the startup race where buildSlash takes
// its initial server snapshot before the asynchronous launcher finishes. The
// launcher-done channel is immutable after runtime construction, so waiting on
// it is safe alongside WaitForMCP and Cleanup.
func (r *runtime) scheduleMCPPromptsAfterLaunch(registry *slash.Registry) {
	if r == nil || registry == nil {
		return
	}
	done := r.mcpLauncherDone
	r.startMCPPromptTask(func(ctx context.Context) {
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return
			}
		}
		r.mcpServersMu.Lock()
		if r.mcpClosing {
			r.mcpServersMu.Unlock()
			return
		}
		snapshot := append([]*mcptools.Server(nil), r.mcpServers...)
		r.mcpServersMu.Unlock()
		for _, server := range snapshot {
			r.registerLiveMCPPrompts(ctx, registry, server)
		}
	})
}
