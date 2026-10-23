# Managed Computer Use

METIS owns delivery and session lifecycle; `metis-cu` is an independent native
MCP executable. The embedded `computer-use` skill describes tool selection,
observation, permissions, and outcome verification. The skill does not replace
the executable or grant access.

## Documentation, skill, and prompt

This file (`docs/computer-use.md`) is human-facing documentation; it is not
injected into model requests. `internal/agent/skills/builtin/computer-use.md` is
the embedded skill: a short metadata header and operating instructions, exposed
through the `Skill` tool. Listing skills returns names and descriptions; getting
or invoking this skill returns its body as tool output. That output remains in
conversation history until compaction, but is not appended anew on every turn.
Tool availability alone does not prove that a model has loaded the skill.

The separate `internal/runtime/prompts/base/01b_computer_use.md` is the existing
conditional system-prompt safety section. It is not a duplicate of this document
or the complete skill. Neither kind of instruction replaces native permission
checks, input serialization, or the cleanup acknowledgement protocol.

## Setup and controls

`metis cu status --json` reports the installation and configured startup state
without starting a server, capturing the screen, or requesting OS permission.
Only `/cu status` inside a running CLI, or the Desktop settings page, can report
the connection owned by that runtime.

| Action | CLI session | Desktop settings | Standalone CLI |
|---|---|---|---|
| Inspect | `/cu status` | Refresh | `metis cu status --json` |
| Install compatible helper | `/cu install` | Install | `metis cu install` |
| Enable | `/cu enable` | Enable | `metis cu enable` (next session) |
| Stop current connection | `/cu stop` | Stop | Not supported across processes |
| Stop and disable startup | `/cu disable` | Disable | `metis cu disable` (future startup only) |
| Open Accessibility settings | `/cu permissions-accessibility` | Accessibility settings | `metis cu permissions-accessibility` |
| Open Screen Recording settings | `/cu permissions-screen-recording` | Screen Recording settings | `metis cu permissions-screen-recording` |

Enabling is explicit. Installation alone does not connect the helper. Permission
controls open the corresponding macOS settings page but do not grant permission.
After granting permission, stop/enable the helper and refresh status. Native
permissions can differ between launch environments and app signatures.

## Delivery and local development

The catalog in `internal/computeruse/releases.json` retains reviewed, published
helper artifacts so local CLI and Desktop rebuilds keep official installation
working. The current source pins helper `0.0.3`, published with METIS `v0.4.90`,
for both macOS architectures. These archive hashes were checked against the
official release asset metadata before deriving the executable hashes from the
authenticated archives. Do not invent hashes or point at an arbitrary latest
release. Official entries pin the helper version, platform, architecture,
archive SHA-256, executable `binarySha256`, and executable path within the archive.
The installer checks both hashes before probing executable capabilities, stages
privately, and resolves the current METIS build's immutable version directory.
Different METIS builds sharing one home do not silently substitute each other's
official helper versions. Archive extraction is bounded and rejects unsafe paths.

For explicit local development:

```sh
# In the metis-cu repository; native build dependencies are required.
go build -o /absolute/path/to/metis-cu .
# In METIS; this executes the selected binary's descriptor probe.
metis cu install --from /absolute/path/to/metis-cu
metis cu enable
```

Local selection is intentionally labeled experimental and takes precedence over
official selection. The local binary is copied into a private, content-addressed
directory; protocol and required lifecycle capabilities are verified. A checksum
of user-selected code detects later changes, not whether that code is trustworthy.

Managed MCP configuration uses the reserved `@metis/computer-use` command under
the `computer-use` server name. The runtime resolves it to the verified absolute
path. Arbitrary environment/argument overrides are not accepted on that managed
entry. Existing custom entries are left intact and produce an explicit conflict;
inspect and remove the old entry deliberately before adopting managed delivery.
Legacy Computer Use entries retain their existing desktop-environment
compatibility but do not receive the new managed input-ownership lock grant.
The managed profile additionally verifies the installed absolute executable at
the stdio transport boundary; selecting a profile alone is not sufficient.

The companion's `docs/component-distribution.md` describes artifact generation.
Publishing, signing/notarization, supported-platform verification, and reviewing
new catalog entries remain release prerequisites, not automatic effects of a
local build. Local packaging can reuse the already published, pinned archives;
it does not need to publish a new component or select a local experimental helper.

Stable releases now make those prerequisites explicit in CI. The release workflow
checks out the exact helper commit recorded in `.github/computer-use-release.json`,
runs the helper tests and a native `--describe --json` protocol gate, builds both
macOS targets, and generates deterministic archives plus archive and executable
SHA-256 values. The protocol gate requires the descriptor, protocol version, and
the `status`, `stop`, `end-turn`, `serialized-input`, and `input-ownership`
capabilities that METIS uses for safe lifecycle control. A plain MCP server that
only happens to expose desktop tools is rejected before any archive or catalog is
written. The generated catalog is copied into the CLI build before Go
compilation, and the same archives are verified offline into the macOS Desktop
bundle before signing. The four helper archive assets are included in the stable
release inventory; CLI-only releases intentionally do not build or publish them.
The checked-in catalog keeps source builds usable between stable releases. CI
replaces it with the newly generated, protocol-compatible two-target catalog for
the release being built; the release build remains blocked if generation fails.
Catalog availability tests prevent the source catalog from silently becoming
empty again. Explicit empty-manifest tests still verify that an unpinned build
cannot trust a self-declared official activation.

Desktop packaging includes the exact pinned release archives under
`Contents/Resources/computer-use` before signing the application. On install or
enable, the host adopts the matching bundled archive offline; the bundle path is
only a hint and never overrides the compiled hashes. A missing archive can use
the pinned download; a corrupt or symlinked archive fails without network fallback.
Previously enabled managed entries adopt the current build's pin at startup.
CLI first use follows the same installer without requiring a Desktop bundle.

Descriptor probes also run in a mandatory credential-isolating sandbox, with a
fixed minimal environment, blocked network, and a private temporary directory.
The generic macOS profile still allows noncredential reads and some system IPC;
it is not a comprehensive no-GUI sandbox for arbitrary untrusted executables.

## Structured operation and evidence

- Connected browser pages expose target/outline-scoped refs and unique escaped
  selectors. Inputs use CDP events after visibility, editability and hit checks;
  text is read back and an optional bounded DOM condition can verify clicks.
- macOS `native_ax_snapshot`, `native_ax_press`, and `native_ax_set_value`
  provide bounded foreground-window accessibility access with explicit app
  grants. Refs bind process instance/window/snapshot, expire after 30 seconds,
  and are consumed after one action. Secure fields and system login UIs are
  rejected. Other OSes report unsupported AX instead of pretending it succeeded.
- Results distinguish dispatch (`not_sent`, `sent`, `unknown`) from verification.
  AXPress acknowledges dispatch only; it does not prove the application completed
  the task. Failed/unknown writes stop a batch without repeating input.
- `computer_batch` defaults to one final observation, supports `each`/`none`,
  retains explicit image steps and labels their zero-based indices. Failure
  retains preceding observations; an image alone never proves task success.
  Image budgets stop further steps with explicit truncation. Obtain new refs
  through standalone snapshot/outline calls, not summarized batch output.

## Lifecycle and safety boundaries

- CLI and Desktop call the same runtime controller. Stop/disable fence pending
  launches so an older completion cannot reconnect the stopped component.
- Managed close requests cleanup and waits for its acknowledgement before
  terminating the connection, including replacement and permission revocation.
- Turn completion/cancellation sends an end-turn boundary that cancels queued
  work and releases held input. The next turn can reuse the same connection.
- The helper serializes native input and holds a machine-wide process lease.
  A second helper reports the ownership conflict; it does not drive the desktop
  concurrently. Stop the owning session before enabling another one.
- Cleanup waits are bounded. A native call may not be interruptible; a timeout
  closes the process and reports cleanup as unconfirmed, not successful.
- OS permission and per-application access are separate checks. Existing app
  grants may persist in `~/.metis-cu/granted.json`; stop is not grant revocation.
- Tools report failed post-action observation as unverified. The skill tells the
  model to inspect before retrying an action that may already have taken effect.

## Desktop interaction enhancements

The companion source includes `activate_application` for switching to an
already running macOS application by its observed PID and bundle ID. It checks
the target's explicit application grant, binds the activation to its process
instance, and verifies the foreground identity. It neither launches an app by
name nor chooses a particular window. After a successful switch, take a fresh
native AX snapshot and verify the window before sending input. Unsupported
platforms return an unavailable result.

On macOS, the helper's main OS thread now runs the application event loop while
the MCP server runs in the background. This keeps foreground application state
fresh across switches. When Accessibility is already authorized, authorization
and native AX checks also read the focused PID synchronously; a failed read
does not fall back to a stale cached identity. Metadata and explicit permission
commands do not start the server event loop.

`screen_size` reports the actual selected display. Screenshot results carry the
display origin, logical dimensions and encoded image dimensions, so a model can
map image coordinates into the global logical coordinate space used by mouse
tools. A missing selected display fails instead of silently capturing the
primary display. Explicit screenshot display selection remains sticky; obtain
fresh geometry after changing displays.

Runtime status separates healthy `running` from host-owned `stoppable` state.
An unhealthy connection, damaged installation metadata or configuration failure
does not remove Stop for a helper the runtime already owns. HTTP error responses
retain partial status, and Desktop permission inspection remains independent of
helper health. Stop does not change the saved enable preference.

These companion changes require a matching helper build. A local development
checkout and an immutable helper pin are separate delivery states; building
Metis alone does not add new tools to an older helper. Native fixture acceptance
and multi-display regression evidence must be reported separately from release
signing and notarization.

## Validation

The targeted tests cover immutable version selection, archive/executable pins,
unsafe extraction, installation locking, descriptor capabilities, lifecycle
acknowledgement and timeout handling, concurrent close, stale launch rejection,
turn cleanup, strict HTTP actions, and Desktop stop/enable response ordering.

```sh
go test ./internal/computeruse ./internal/runtime/mcp ./internal/mcp ./internal/tools/mcp
go test ./cmd/metis ./internal/tui ./internal/webui ./internal/agent/skills
go test -race ./cmd/metis ./internal/agent -run 'TestComputerUse|TestLoopComputerUse'
```

An explicitly opt-in `TestComputerUseLiveCLI` runs a compiled CLI in a temporary
METIS home, using the configured provider and local helper. It requests only
screen dimensions and cursor position, never screenshots or input. It requires
`METIS_CU_LIVE_TEST=1`, `METIS_CU_TEST_CLI`, and `METIS_CU_TEST_HELPER`. It must not
be treated as passed unless a real provider completes and native tool calls are
recorded. Select an authorized API-key route with `METIS_CU_LIVE_PROVIDER`, or
explicitly opt into an isolated Codex OAuth test with `METIS_CU_LIVE_OAUTH=1`.
The OAuth path requires at least ten minutes of remaining validity, stages only
the selected credential in a private temporary directory, bounds execution before
the refresh window, and verifies that neither original nor temporary credentials
changed. Native Desktop clicks, permission denial/regrant, action cancellation,
signed distribution, and other OS/architecture combinations require separate
explicit interactive validation.
