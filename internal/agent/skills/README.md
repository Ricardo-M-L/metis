# internal/agent/skills

The skill subsystem. A "skill" is a packaged unit of agent capability:
a markdown manifest with YAML frontmatter (name + description + tools
allowed + permission scope) plus optional inline shell or auxiliary
files. Skills load from multiple sources (bundled / optional / user /
project / plugin / MCP), get filtered by safety, and become tools the
model can invoke via `Skill(name="…")`.

## File-naming convention

| File | What it owns |
|---|---|
| `loader.go` | Multi-source loader. `Layer` is one priority-tagged source; higher priority wins on name collision (bundled < optional < user < project < plugin < mcp). |
| `manifest.go` | YAML-frontmatter parser. Validates `name`, `description`, `tools`, `permission_mode`, `effort`, `max_turns` fields. |
| `store.go` | In-memory registry of loaded skills. The Tool implementation queries this. |
| `search.go` | Skill-name search + fuzzy matching for the `Skill(name="…")` tool input. |
| `safety.go` | Pre-load safety filter: reject manifests that ask for `permission_mode: bypass` from untrusted sources, or that include suspicious shell. |
| `marketplace.go` | `metis skills install <name>` plumbing — fetches optional skills from the marketplace registry. |
| `embedded.go` | The bundled (compiled-in) skill set — defaults that ship with metis. |
| `inline_shell.go` | Parses `<inline:bash>...</inline:bash>` blocks inside a skill manifest body. Pre-Skill-tool execution. |
| `expand.go` | Renders a skill body template with arg substitution before handing to the LLM. |
| `filter.go` | Post-load filter (per-session enable/disable, per-tool allowlist). |
| `synth.go` + `synth_tool.go` | The `metis skills synth <name>` path — generate a new skill from a natural-language description, dispatched as a Synth subagent. |

12 prod + 12 test files. Each file is its own well-defined concern;
no further sub-package split is warranted.

## Where do I find...

- **How a skill is loaded** → `loader.go::Load`, which calls each
  `Layer.Scan` in priority order.
- **Manifest YAML schema** → `manifest.go` struct + validator.
- **What makes a skill "safe"** (the gate before it can run) →
  `safety.go`.
- **Skill installation from marketplace** → `marketplace.go`.
- **Bundled built-in skills** (compiled into the binary) →
  `embedded.go` + `internal/agent/skills/builtin/` subdir.
- **Synthesizing a new skill from a prompt** → `synth.go` / `synth_tool.go`.

## Design invariants

- A skill manifest with `permission_mode: bypass` from an untrusted
  source (anything outside `bundled` and `optional`) is **dropped at
  load time** with a warning. The safety filter never silently
  approves bypass for user/project/plugin/mcp skills.
- Priority resolution is **deterministic**: higher priority always
  wins, ties broken by source path (sorted ascending). No "first
  wins" / "last wins" surprises.
- Each `Layer.Scan` returns a snapshot — loader holds no live
  references to per-layer state, so a skill rescan doesn't risk
  reading mid-write data.
