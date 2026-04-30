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
```

Pre-commit checklist:

1. `go test ./...` is green
2. `go vet ./...` is clean
3. `gofmt -l .` returns nothing
4. The change has either a test or a clear "tested manually because <reason>" note

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
