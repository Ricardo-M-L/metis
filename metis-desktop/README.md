# Metis Desktop

The native Metis client is built with Wails. Go owns the application backend,
workspace-scoped session access, settings, and Metis CLI execution; `frontend`
contains the WebView UI. Files under `frontend/wailsjs` are generated Wails
bindings for exported Go methods and should not be edited by hand.

## Prerequisites

Building both the root CLI and the desktop application requires:

- Go 1.25.8 or newer. The desktop module itself declares Go 1.23, but the root
  Metis CLI currently declares Go 1.25.8.
- Node.js `^20.19.0` or `>=22.12.0` and npm, as required by the pinned Vite
  frontend dependency.
- Wails CLI v2.12.x and the platform dependencies required by Wails. Install the
  matching CLI with `go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0`
  and use `wails doctor` to check the host toolchain.
- A configured Metis CLI and provider credentials. The desktop delegates chat,
  session, model, and scheduler operations to the CLI; it is not a standalone
  inference runtime.

`METIS_HOME` controls the Metis data directory used by both processes. It
defaults to `~/.metis`; desktop settings are stored in
`$METIS_HOME/desktop-settings.json`.

## Build and run from the repository

Run the following from the repository root:

```sh
npm ci --prefix metis-desktop/frontend
make build
(
  cd metis-desktop
  wails build
)
./bin/metis desktop
```

`make build` is preferred over a bare `go build`: it injects the CLI version,
commit, and build date. `wails build` also invokes the frontend install and build
commands declared in `wails.json`; the explicit `npm ci` above first verifies
and materialises the committed lockfile.

This source-tree launch works because `metis desktop` is run from the repository
root and can find `metis-desktop/build/bin`. To launch the locally built app from
another workspace, either install the native application in a platform-standard
location or set `METIS_DESKTOP_APP` to its absolute path before running
`metis desktop`.

The launcher passes the exact CLI path and current working directory to the
native app. If the application is started directly instead, set `METIS_BIN` to
an absolute path to an executable Metis CLI, or ensure `metis` is on `PATH`.

## Native application and browser UI

```sh
metis desktop                         # native Wails application; no web UI port
metis desktop --web                   # browser UI on 127.0.0.1:8080
metis desktop --web --port 9090       # browser UI on 127.0.0.1:9090
METIS_PORT=9090 metis desktop --web   # env fallback when --port is omitted
```

`--port` (or `-p`) implies `--web`. `METIS_PORT` is only consulted for web mode
when no explicit port was supplied. The native application does not expose this
HTTP server; Wails development mode may use its own internal development server
for frontend reloads.

## Development

Build the CLI first and give Wails an absolute path to it:

```sh
make build
export METIS_BIN="$PWD/bin/metis"
cd metis-desktop
wails dev
```

When launched directly with `wails dev`, the desktop workspace defaults to the
`metis-desktop` directory. A normal `metis desktop` launch instead passes the
caller's current directory as the workspace.

Wails development mode uses Vite for live frontend reloads. Normal Go tests do
not require `frontend/dist`; production builds generate and embed that directory
before compiling the application.

## Verification and production build

From the repository root:

```sh
go test ./internal/desktop ./cmd/metis
(
  cd metis-desktop
  go test ./...
  go test -race ./...
)
npm ci --prefix metis-desktop/frontend
npm run check --prefix metis-desktop/frontend
(
  cd metis-desktop
  wails build
)
```

The native application bundle and the root CLI are separate artifacts. Release
automation must keep their user-visible version metadata aligned; the VS Code
extension under `editors/vscode` is versioned independently.
