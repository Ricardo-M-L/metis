# Metis IDE bridge (VS Code)

Lets the **Metis CLI** read your editor state and show inline diffs while
it works — the metis equivalent of Claude Code's IDE integration.

## How it works

```
VS Code extension (this)
 └─ MCP server (Streamable HTTP, loopback only)
     ├─ getCurrentSelection()   → active editor selection + location
     ├─ getDiagnostics(uri?)    → compiler/linter problems
     └─ openDiff(path,newText)  → inline diff + Accept/Reject, applies on Accept
           ▲
           │ http://127.0.0.1:<port>/mcp   (Bearer token)
           │ discovered via ~/.metis/ide/<port>.lock
           │
 metis CLI ── auto-attaches when launched in this workspace
```

On activation the extension:

1. starts the MCP server on a free loopback port,
2. writes `~/.metis/ide/<port>.lock` (pid, port, workspace folders, auth token).

When you run `metis` in a terminal whose working directory is inside this
workspace, it reads the lockfile, connects, and registers the three tools
as `mcp__ide__*`. Check with `/ide` inside metis.

> Transport note: Claude Code's IDE bridge uses WebSocket; metis reuses
> its existing Streamable-HTTP MCP client instead (both are valid MCP
> transports), so this extension hosts an HTTP server, not a ws one.

## Develop

```bash
cd editors/vscode
npm install
npm run build        # bundle to dist/extension.js
npm run typecheck    # tsc --noEmit
# Then press F5 in VS Code to launch an Extension Development Host.
```

## Settings

- `metis.ide.enabled` (bool, default true) — host the bridge.
- `metis.ide.port` (number, default 0) — fixed port, or 0 for a free one.
