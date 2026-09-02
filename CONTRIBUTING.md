# Contributing to Metis

Thanks for your interest in helping out. This document collects the conventions
the maintainer (and any contributors) follow for code, commits, and reviews.

A Chinese version is available at [CONTRIBUTING.zh-CN.md](CONTRIBUTING.zh-CN.md).

## Project layout

```
cmd/metis/                 CLI entry + one file per subcommand
                           (auth/diag/plugin/stats/trust/…)
internal/agent/            the message → tool → message loop (Loop +
                           dispatch + detectors + verdict gate +
                           contract + orphan repair)
internal/agent/skills/     SKILL.md loader + bundled skills
internal/agent/transcript/ per-run transcript persistence
internal/tools/            Tool interface + registry
internal/tools/builtin/    first-party tools (Read/Write/Edit/Glob/
                           Grep/LS/Git/WebFetch/WebSearch/WebBrowse/
                           NotebookEdit/Todo/Ask/LSP/Agent/Fork/Task*/
                           plan-mode/Skill/Memory/MetisInfo/Monitor/
                           ScheduleWakeup/MessageTeammate/SendMessage)
internal/tools/builtin/bash/  Bash family (Bash + List/Output/Kill
                              jobs + classifier + security rules)
internal/llm/              provider clients (Anthropic, OpenAI, Gemini,
                           Azure, Bedrock, Vertex, Cloud, custom)
internal/llm/transport/    shared HTTP client + retry/dump/log/overflow
internal/memory/           multi-tier memory (Core / Archival / Recall + Daily)
internal/runtime/          bootstrap glue: build provider, build registry,
                           build loop
internal/runtime/mcp/      MCP registry + cache + prompts collector
internal/tui/              bubbletea chat surface (split by concern,
                           single Model)
internal/tui/screen/       full-screen overlays (help/history/…)
internal/slash/            slash-command registry + handlers
internal/permission/       cascading gate (default/acceptEdits/plan/dontAsk/bypassPermissions/fullAccess)
internal/exitcode/         typed errors → shell exit codes
internal/jobs/             background process pool for the Bash family
internal/channels/         chat-platform adapters (Slack/DingTalk/…)
internal/mcp/              stdio + Streamable HTTP/SSE clients (SDK shape)
acp/                       Agent Client Protocol JSON-RPC server
pkg/                       stable public API (tool, memory, plugin,
                           skill, hook, channel, provider, session, llm)
docs/                      architecture + design notes
```

Anything under `internal/` is implementation detail. Anything under `pkg/` is a
contract — break it only with a deprecation cycle.

Several internal packages carry a `README.md` documenting their
file-naming convention and "where to find X" pointers — start there
when navigating a cluttered package (`internal/tui/`, `internal/agent/`,
`internal/tools/builtin/`, `internal/runtime/`, `internal/llm/transport/`,
`internal/slash/`, `internal/agent/skills/`).

## Building & testing

Use the Go toolchain declared by `go.mod`; CI reads that file rather than
duplicating a version number in this guide.

Root module checks (required for every code change):

```sh
gofmt -l .                              # must print nothing
go vet ./...
go build ./...                          # compile every root-module package
go test -count=1 -timeout 90s ./...
```

Common local build and install targets:

```sh
make test                               # optional root-module race + coverage run
make build                              # versioned CLI at ./bin/metis
make install                            # one local binary; refuses a managed release
```

If the curl-installed release already owns `~/.local/bin/metis`, run the
source build as `./bin/metis`. The Make target deliberately does not call
`go install`, because a second binary in `~/go/bin` can shadow the managed
launcher and remain stale after automatic updates.

The repository also contains nested modules that root-level `./...` does not
cross. Run the matching checks when those areas change; CI runs all of them:

```sh
(cd vendor-patches/bubbletea-v2 && go vet ./... && go test -race -count=1 ./...)
(cd vendor-patches/ultraviolet && go vet ./... && go test -race -count=1 ./...)

(cd metis-desktop/frontend && npm ci && npm run check && npm run build)
(cd metis-desktop && go vet ./... && go test -race -count=1 ./... && go build -tags production ./...)
```

GitHub Actions repeats the root-module formatting, vet, build, and test checks
on Ubuntu, macOS, and Windows. The desktop job runs on macOS.

End-to-end (TUI behaviour) — uses tmux to drive a real `metis chat`
session and `capture-pane` to assert visible state:

```sh
scripts/e2e/tmux_drive.sh --list        # list available cases
scripts/e2e/tmux_drive.sh slash_help    # run a single case
scripts/e2e/tmux_drive.sh               # run the whole pack
```

Captures land in `${METIS_E2E_OUT:-/tmp/metis-e2e-tmux}/`. The older
`scripts/e2e/macos_drive.sh` (osascript / Terminal.app) still works
on macOS but isn't required — the tmux driver runs headless and on
Linux too.

For parity work against claude code, `scripts/e2e/cmp_drive.sh`
launches both binaries side-by-side, sends matching inputs, and
records each case's two pane captures + a Markdown triage line:

```sh
scripts/e2e/cmp_drive.sh --list         # list comparison cases
scripts/e2e/cmp_drive.sh slash__help    # single case
scripts/e2e/cmp_drive.sh                # full pack (~3 min)
```

Output: `/tmp/metis-cmp-captures/*.txt` (full pane dumps) and
`/tmp/metis-cmp-issues.md` (triage). Fail loud, fix, re-run.

Pre-commit checklist:

1. The root-module format, vet, build, and test commands above pass.
2. Checks for every touched nested module or frontend area pass.
3. TUI-facing changes pass the relevant tmux case (and parity case when
   applicable), or the PR explains why they were tested manually.
4. New behaviour has a test, or a clear "tested manually because <reason>" note.
5. Behaviour changes update the relevant docs in the same PR (README flag /
   slash / keybind tables, `CHANGELOG.md` Unreleased entry, and
   `docs/ARCHITECTURE.md` when architecture or package boundaries change).

## Style

- **Comments explain WHY, not WHAT.** A well-named function does not need a
  paraphrase of its body in a docstring.
- **No multi-paragraph docstrings.** One short line is the cap.
- **No backwards-compat shims for unreleased changes.** This is a 0.x project;
  break things cleanly.
- **No emoji unless the user asks.**
- **Avoid global state.** New subsystems take dependencies via struct fields.

For Go specifics, follow [Effective Go](https://go.dev/doc/effective_go) and the
patterns already present in the repo.

## Commits

```
short imperative summary (under 70 chars)

Optional body explaining the *why*. Wrap at 72.
Reference issues with `Fixes #N` / `Refs #N` in a trailing line.
```

Squash-merge is the default; fork branches don't need to be tidy.

## Pull requests

1. Open an issue first for non-trivial changes — get directional alignment
   before writing code.
2. Keep PRs focused. One concern per PR; refactors separate from features.
3. Title follows commit style.
4. Description should answer: what, why, how was it tested.
5. CI must be green before merge.

## Reporting bugs

Use the bug template in `.github/ISSUE_TEMPLATE/`. Attach:

- `metis version -V` output
- A minimal reproduction (config snippet, exact command, expected vs actual)
- Logs with `METIS_DEBUG=1` set

## Security issues

Don't open a public issue for security problems. See [SECURITY.md](SECURITY.md).

## Code of conduct

This project follows the [Contributor Covenant 2.1](CODE_OF_CONDUCT.md).
By participating you agree to abide by it.
