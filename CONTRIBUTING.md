# Contributing to Metis

Thanks for your interest in helping out. This document collects the conventions
the maintainer (and any contributors) follow for code, commits, and reviews.

A Chinese version is available at [CONTRIBUTING.zh-CN.md](CONTRIBUTING.zh-CN.md).

## Project layout

```
cmd/metis/         CLI entry, subcommand dispatch, flag wiring
internal/agent/    The message → tool → message loop
internal/tools/    Tool registry + 16 built-in tools (Bash, Read, Edit, ...)
internal/llm/      Provider clients (Anthropic, OpenAI, Gemini, custom)
internal/memory/   Multi-tier memory (Core / Archival / Recall + Daily)
internal/runtime/  Bootstrap glue: build provider, build registry, build loop
internal/tui/      bubbletea chat surface (50+ files, single Model)
internal/permission/  Allow / Deny / Ask gating
acp/               Agent Client Protocol JSON-RPC server
pkg/               Stable public API (tool, memory, plugin, skill)
docs/              Architecture + design notes
```

Anything under `internal/` is implementation detail. Anything under `pkg/` is a
contract — break it only with a deprecation cycle.

## Building & testing

```sh
go build ./...                          # binary at ./metis
go test -count=1 -timeout 90s ./...     # full unit suite (~30s)
go vet ./...                            # default vet checks
make install                            # publish to ~/.local/bin/metis + ~/go/bin/metis
                                        # (run after every code change so tmux e2e + manual smoke
                                        # both pick up the new binary)
```

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

1. `go test ./...` is green
2. `go vet ./...` is clean
3. `gofmt -l .` returns nothing
4. The change has either a test or a clear "tested manually because <reason>" note
5. Touched docs in the same PR if behaviour changed (README's flag /
   slash / keybind tables, `CHANGELOG.md` Unreleased entry,
   `docs/ARCHITECTURE.md` if a package moved)

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
