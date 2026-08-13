# `internal/slash`

This package owns the signal-based slash-command registry and the loader for
Markdown prompt commands. It is not the only command dispatcher: interactive
Metis currently has two registries, and routing order matters.

## Runtime routing

1. `internal/tui.REPLCommandRegistry`, built in
   `internal/tui/commands.go`, owns commands that need the live `*REPL`. Its
   handlers may change live state and return display text directly.
2. `internal/slash.Registry`, populated by `slash.RegisterAll` plus runtime
   registrations, owns signal-based commands. Its handlers return
   `(display, Signal)` for the TUI or plain REPL to interpret.

Both interactive dispatchers normally consult REPL commands first, then slash
commands. Ownership exceptions are surface-specific: the Bubble Tea TUI uses
the slash implementation for `memory`, `doctor`, `diff` (including
`diff-view`), and `init`; the plain readline REPL keeps its original
`memory read|write|search|clear` handler while preferring slash for the other
three. The merged TUI catalog in `internal/tui/command_catalog.go` applies the
same ownership and alias rules to `/help`, completion, and the command palette.

Do not maintain a copied list of every command here. The live `/help` / command
palette is the user-visible inventory, and the registries plus their routing
tests are the implementation authority.

## Package map

| File | Responsibility |
|---|---|
| `commands.go` | `Cmd`, `Handler`, `Signal`, `Registry`, aliases/reservations, built-in signal commands, and `/reload`. |
| `custom.go` | User and project Markdown commands, frontmatter parsing, template expansion, trust boundaries, and custom-command reload support. |
| `debug.go`, `review.go`, `init.go` | Specialized prompt-producing commands kept out of the main registration body. |
| `batch_prompt.go` | Prompt construction for `/batch`. |
| `cron_handler.go`, `loop_handler.go`, `memory_cmd.go` | Feature-specific command implementations. |
| `midturn.go` | Classification of slash input received while a model turn is active. |

The other half of the system lives in `internal/tui/commands.go` (direct REPL
commands), `internal/tui/keybind_submit.go` and `internal/tui/repl.go`
(dispatch), and `internal/tui/command_catalog.go` (merged discovery). Runtime
wiring, reservations, custom loading, and MCP prompt registration happen in
`cmd/metis/main.go`.

## Custom Markdown commands

Commands are loaded from these directories, in order:

- `$METIS_HOME/commands/*.md` (normally `~/.metis/commands/*.md`): user-level,
  trusted commands.
- `<cwd>/.metis/commands/*.md`: project-level, untrusted commands; a project
  file may replace a same-named user custom command.

The filename without `.md` is the command name. Optional flat frontmatter
recognizes `description`, `argument-hint`, `allowed-tools`, and `model`. The
body is a prompt template: `$ARGUMENTS` and `$1` through `$9` are expanded for
both trust levels.

Trust changes behavior:

- A trusted user command may execute `` !`shell command` `` and inject the
  command's output. Each substitution has a five-second limit and uses the
  runtime sandbox when one is wired.
- A trusted user command may replace `@path` with a readable file's contents.
- Trusted `allowed-tools` metadata can install one-turn permission rules.
  Trusted `model` metadata must be `inherit` or exactly match the active model;
  it does not switch models automatically.
- Project commands leave shell and file substitutions literal. Their
  `allowed-tools` and non-`inherit` `model` metadata are ignored with a
  warning, while normal permission checks remain active.

Invoking a trusted custom command can therefore perform shell and file I/O;
slash handlers are not generally pure functions. The model-facing
`SlashCommand` tool accepts custom commands only and refuses built-ins, whose
signals require a UI caller.

## Name conflicts

- Built-in slash names and aliases are protected from custom commands.
- `cmd/metis` also reserves every REPL command name and alias before scanning
  `commands/*.md`, so a custom command cannot load successfully while being
  unreachable behind the first dispatcher.
- Conflicting custom files are skipped silently; no warning is logged.
- Project custom commands are the one deliberate exception: because they load
  after user custom commands, a project command with the same name wins.
- Runtime sources such as MCP prompts add names dynamically. Use distinct,
  preferably namespaced custom names instead of depending on a collision with
  a runtime registration.

## Reloading

`/reload` returns `SignalReload`. The TUI and plain REPL then invalidate the
disk-backed skill catalog, call `Registry.RemoveCustom()`, and rescan both
custom-command directories. Built-ins and other non-custom runtime commands
remain registered, while edited and deleted Markdown files take effect without
restarting Metis. `/reload` is not a general configuration or MCP-server
restart.

## Adding or changing a command

- For a direct UI/REPL operation that needs live state, register a
  `REPLCommand` in `internal/tui/commands.go`.
- For signal-based behavior, register a `Cmd` in `RegisterAll` or a focused
  file in this package, then implement the signal in both the Bubble Tea and
  plain-REPL dispatch paths where it is supported.
- For a user-authored prompt recipe, add a Markdown file under one of the
  `commands/` directories rather than changing Go code.
- For behavior while a model response is in progress, update `midturn.go` and
  the relevant queue/steering tests.

When a name exists in both registries, make the intended owner explicit and
cover it in `internal/tui/command_catalog_test.go` and the slash end-to-end
tests. Do not assume that adding a signal handler makes it reachable if a REPL
command already owns the name.
