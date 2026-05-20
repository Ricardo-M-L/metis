# internal/slash

Slash-command system. The TUI (and headless REPL via `metis chat`)
routes any input starting with `/` through here: parse the name,
look up the handler in the registry, run it, and return a `Signal`
that tells the caller what to do next (insert text, send to model,
clear, quit, etc.).

## File-naming convention

| File | What it owns |
|---|---|
| `commands.go` | The core types: `Cmd`, `Handler`, `Signal`, and the registry that resolves a name to a handler. Built-in slash commands register here. |
| `custom.go` | User-authored slash commands loaded from `~/.metis/slash/*.md` (a Markdown body becomes a prompt template; YAML frontmatter sets the name + description + args). |
| `midturn.go` | Mid-turn signal classification — when the user types while the model is still streaming, classify whether to interrupt / queue / steer. |
| `batch_prompt.go` | The multi-stage prompt template for `/batch <task>` — fans out N sub-tasks via the Agent tool. |
| `cron_handler.go` | `/cron` subcommands: list / add / pause / resume / rm / run / start. |
| `loop_handler.go` | `/loop` subcommands. Tags every spawned job with a `loop:` prefix so `/loop list` and `/loop stop` can find them again. |
| `memory_cmd.go` | `/memory` subcommands: list / read / write / clear, gated on the memory subsystem being enabled. |

7 prod + 7 test files. Each handler file is its own concern; the
registry in `commands.go` is the meeting point.

## Where do I find...

- **Adding a built-in slash command** → register in `commands.go`'s
  table (or a feature-specific handler file like `cron_handler.go`).
- **A user adding their own `/foo`** → drop a Markdown file in
  `~/.metis/slash/`; `custom.go` loads at REPL startup.
- **What happens when user types `/x` mid-stream** → `midturn.go`
  classifies as interrupt / queue / steer.
- **The Signal enum** (return value of every handler) → `commands.go`.

## Design invariants

- Handlers are **pure functions** of `(args string) → (display, Signal)`.
  Side effects on the REPL state happen via the returned `Signal`,
  not by reaching into the caller's struct.
- The registry is **resolved at REPL startup** — adding/removing
  commands at runtime isn't supported (custom slash files are
  re-read only on REPL restart, mirroring claude-code's pattern).
- Built-in slash commands take precedence over custom ones with the
  same name (no override). Custom names that collide log a warning
  during load.
