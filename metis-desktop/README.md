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
`$METIS_HOME/desktop-preferences.json`.

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
when no explicit port was supplied. The native shell starts a random loopback
server and displays it in an embedded frame. The shell keeps a deliberately
narrow native bridge for the system folder picker and explicit in-app updates;
the browser-only build receives neither capability.

## Agent concurrency

Desktop text turns run in isolated Metis worker processes, including interactive
permission modes: approval requests and replies travel through each worker's
private channel. A single workspace has one foreground writer lease, so two
conversations cannot concurrently change the same checkout. Image turns retain
the serialized in-process runtime while sharing the Desktop execution budget.

**Settings → General → Agent concurrency** exposes three independent limits:

| Setting | Default | Range |
| --- | --- | --- |
| Concurrent foreground workspaces | 8 | 1–12 |
| Running agents, roots and children combined | 16 | 1–64 |
| Running children per root task, including descendants | 8 | 1–32 |

Named and anonymous children use the same pool. Background spawns return an
agent ID immediately even when capacity is full; queued agents start as permits
become available and can be inspected or cancelled from the child-agent panel.
There is a separate safety ceiling of 256 outstanding children per root. Roots
waiting for child work release their execution permit, including during nested
delegation. Admission rotates between roots and preserves eligible FIFO order
within each root. Desktop-launched cron processes share this pool; independently
launched CLI sessions retain their existing concurrency configuration.

Settings persist in `METIS_HOME/desktop-preferences.json`. Changes apply to new
work immediately when all tasks are idle; with active or queued work, or an
active cron scheduler process, they take effect after restarting Desktop.
`METIS_DESKTOP_MAX_PARALLEL_TURNS` remains a foreground-workspace environment
override for automation and development. Each Desktop backend owns its own pool;
it is not a machine-wide limit across separately launched applications.

Stopping a root cancels its running and queued children. Kernel locks release
execution permits after a worker crash; this does not restart abandoned jobs
or provide durable queue recovery after the application exits.

## In-app updates

The update icon beside Settings performs a read-only release check. Merely
launching Desktop or seeing the green availability dot never downloads or
installs anything. The current version remains active until the user opens the
dialog and chooses **Update and restart**.

After that explicit confirmation, Desktop updates the matching Metis CLI,
downloads the platform Desktop release and its SHA-256 sidecar, verifies the
archive and candidate application, atomically activates it, and restarts with
the same workspace. macOS also verifies the bundle with `codesign` and keeps the
prior bundle at `<application>.previous` as a rollback copy. Linux supports the
same explicit flow for the published amd64 binary. Windows release discovery is
shown but automatic activation is disabled until a signed hand-off helper can
replace the running executable safely.

## Navigation and scheduled tasks

Settings and scheduled tasks are pages inside the main window. Use the back
and forward buttons, `Alt+Left` / `Alt+Right`, `Cmd+[` / `Cmd+]` on macOS, or mouse side buttons to move
through previously visited pages. Opening settings does not stop an active
conversation. Conversation, trajectory, artifact and file views belong to the
selected session; results from an earlier selection must not replace the
currently selected session.

The **Scheduled tasks** page manages the existing durable Metis cron jobs. It
provides search, creation and editing, pause/resume, manual execution, deletion,
and per-run results. Schedules support intervals, cron expressions and one-time
execution, with an explicit time zone. A run has its own identifier and records
its start, finish, outcome and associated conversation when one was created.
Tasks created in Desktop retain that application's workspace directory for
later runs, including runs started by a different Desktop instance. Older CLI
jobs without a saved workspace retain their existing launch-directory behavior.

Scheduled execution is off until explicitly enabled in the page. This setting
is remembered for future Desktop launches. The scheduler runs in the local
backend, so closing the task page does not stop it; quitting Desktop does.
This does not install a system service or guarantee execution while the
computer is asleep. An independently started `metis cron start` process remains
independent of the Desktop scheduler switch.

Unattended runs use the job's tool allow-list and Metis cron permission checks.
They do not inherit an interactive conversation's bypass permission mode.
Pausing or deleting a schedule prevents future scheduled runs; inspect an
already running job's status separately.

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
go test ./internal/webui ./internal/desktop ./cmd/metis
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
