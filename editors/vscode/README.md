# Metis IDE bridge (VS Code)

The extension lets the Metis CLI read editor state and request reviewable inline
diffs. It hosts a local MCP server inside the VS Code extension host; Metis is the
MCP client.

## Prerequisites

- VS Code 1.85 or newer.
- A configured Metis CLI available on `PATH`.
- Node.js 18 or newer and npm when building the extension from source.
- VS Code's `code` shell command on `PATH` for the development-host command
  shown below.

The extension has its own version in `package.json`. It is released independently
of the Metis CLI and Desktop versions; compatibility is defined by the lockfile
and MCP contracts below, not by matching semantic version numbers.

## How it works

```text
VS Code extension
  `- Streamable HTTP MCP server on 127.0.0.1:<port>
       |- getCurrentSelection()    active selection or cursor location
       |- getDiagnostics(uri?)     VS Code diagnostics, optionally for one file
       `- openDiff(path, newText)  full-file proposal with Accept or Reject
              ^
              | POST http://127.0.0.1:<port>/mcp
              | Authorization: Bearer <token>
              | discovered via $METIS_HOME/ide/<port>.lock
              |
          Metis CLI
```

On activation, when `metis.ide.enabled` is true, the extension:

1. listens only on loopback; port `0` asks the operating system for a free
   ephemeral port,
2. creates a random bearer token, and
3. writes a mode-`0600` discovery lockfile containing the PID, selected port,
   workspace folders, IDE name, and token.

`METIS_HOME` defaults to `~/.metis`, so the default lockfile path is
`~/.metis/ide/<port>.lock`. The VS Code extension and Metis CLI must inherit the
same `METIS_HOME` value or they will scan different directories.

Metis discovers the bridge during CLI startup. It prefers the deepest workspace
folder containing the CLI's current directory. If there is no workspace match
but exactly one live IDE lockfile, current discovery logic uses that sole server;
with multiple unmatched servers it does not guess. Start or restart Metis after
the extension bridge is running.

The three tools are registered as `mcp__ide__*`. The `/ide` command reports
whether a matching live lockfile is discoverable; it is not an end-to-end MCP
health check and does not prove that the handshake succeeded. Use the VS Code
command **Metis: IDE Bridge Status** to confirm that the extension-side listener
is running, and consult Metis debug output for connection errors.

`openDiff` treats `newText` as the complete replacement contents of `path`. It
writes only after Accept. Reject, closing the prompt, or no decision within 50
seconds leaves the file unchanged. Increasing the Metis `MCP_REQUEST_TIMEOUT`
alone does not change this extension-side 50-second limit.

## Build and run from source

From the repository root:

```sh
cd editors/vscode
npm ci
npm run typecheck
npm run build
code --new-window --extensionDevelopmentPath="$PWD"
```

`npm run build` bundles the extension to `dist/extension.js`; it does not install
or publish a VSIX. There is currently no checked-in VSIX packaging or Marketplace
publishing script, so this repository documents the development-host workflow
only.

## Settings and commands

- `metis.ide.enabled` (boolean, default `true`) hosts the bridge.
- `metis.ide.port` (number, default `0`) uses a free port; set a valid, unused TCP
  port to make it fixed.
- **Metis: IDE Bridge Status** reports the extension-side listener address.
- **Metis: Restart IDE Bridge** stops the listener, rereads settings, and starts
  it again when enabled.

Settings are read when the bridge starts; they are not hot-reloaded. After
changing `metis.ide.enabled` or `metis.ide.port`, run **Metis: Restart IDE
Bridge** or reload the VS Code window.

The server is loopback-only and requires the bearer token on every MCP request.
Treat the lockfile as a local secret and do not copy it to shared storage.
