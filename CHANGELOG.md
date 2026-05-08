# Changelog

All notable changes to Metis are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added — resume-hint + bare `-r` picker

- After every clean exit (`/quit`, `Ctrl+D`, `Ctrl+C` at idle), metis
  prints a dim gray hint to stderr matching claude-code's format:

  ```
  Resume this session with:
  metis --resume <full-uuid>
  ```

- Bare `metis -r` / `metis --resume` (with no UUID arg) now opens an
  interactive picker over recent sessions. Type the number to resume,
  Enter or `q` to bail to a fresh session. Mirrors `claude -r`.

### Fixed — caught by side-by-side comparison run (cmp_drive.sh)

- `/help` overlay header used to render `metis vv0.1.3-21-gab7a825-dirty`.
  `versionLabel()` was hand-prepending `v` to a string the linker
  already passed in tag form, AND the long git-describe suffix never
  got stripped. Both call sites (help header + bottom status bar) now
  share `version.Short()`.
- `metis version` (no flag) used to print the full git-describe form
  (`v0.1.3-21-gab7a825-dirty (Metis · ab7a825)`) while the bottom
  status bar correctly showed `current: v0.1.3`. The default form now
  matches the status bar; `metis version -V` still prints the full
  build fingerprint for debugging.
- `internal/tui/render_tool.go::renderEditDiff` and `countEditDiff`
  read `input["old_string"]` / `input["new_string"]`, but metis's
  Edit tool actually declares its inputs as `old` / `new`
  (`internal/tools/builtin/edit.go`). Real Edit tool calls produced
  no colored diff and a `Added 0 lines, removed 0 lines` summary.
  Both functions now read `old`/`new` first, falling back to the
  longer claude-code-style names. Discovered when running the live
  end-to-end Edit verification, NOT by the unit tests (which used the
  wrong field names too).

### Added — Claude Code parity push (60 items + UI follow-ups)

#### Slash commands

- `/mcp` subcommands beyond list/add/remove/start: `enable`, `disable`,
  `edit [<name>]` (opens `~/.metis/mcp.toml` in `$EDITOR` and re-validates
  on save), `test <name>` (one-shot connect + tool list), `logs <name>`
  (tails `~/.metis/mcp-logs/<name>.log` when present), `reload`.
- `/skills` is now a full subcommand surface: `list`, `install <ref>`,
  `remove <name>`, `info <name>`, `edit <name>`, `enable <name>`,
  `disable <name>`, `create <name>` (writes a SKILL.json skeleton),
  `search <query>` (local fuzzy match — `/skill search` still hits GitHub).
  Skills can now carry a `disabled: true` field; the loader filters them
  out without deleting the manifest.
- High-frequency commands: `/copy [N]`, `/commit-push-pr <msg>`,
  `/insights [--days=N]`, `/output-style full|streamlined|minimal`,
  `/break-cache` (documents how to flush prompt cache), `/security-review`,
  `/feedback` (alias of `/bug`).
- Discoverability: `/thinkback` (surfaces the most recent assistant
  turn's thinking trace), `/ultraplan` (deep-plan nudge — bumps effort
  to high), `/onboarding` (first-run setup recap).
- User-authored commands: drop `*.md` files under `~/.metis/commands/`
  or `<cwd>/.metis/commands/` — each becomes `/<filename>` with YAML
  frontmatter for description and `$ARGUMENTS` / `$1` / `$2` substitution.
- MCP server prompts auto-discover: any server that implements
  `prompts/list` registers as `/mcp__<server>__<prompt>` slashes.

#### CLI subcommands and flags

- New top-level subcommands: `metis ps` (lists sessions, newest first,
  with pid + size + title), `metis logs <id>`, `metis attach <id>`
  (alias of `chat -r`), `metis kill <id>` (SIGTERM via pidfile).
- Short / long flag pairs: `-c, --continue` (resume newest session),
  `-r, --resume <id>` (the original `-r` "flag provided but not defined"
  bug from the 2026-05-07 video is fixed), `-d, --debug` (mirror logs
  to `~/.metis/debug.log`), `-s, --scope <local|user|project>`.
- New flags: `--bare` (skip MCP + plugin loaders), `--name <text>`,
  `--agent-teams`, `--tmux`, `--input-format json`, `--output-format
  json|stream-json`, `--dangerously-skip-permissions` (alias of
  `--mode bypass`).

#### Keybindings

- `Ctrl+G` — open the current input draft in `$VISUAL` / `$EDITOR` /
  `vi`; save and exit reads the file back into the textarea.
- `Ctrl+X` — toggle shell mode (next submission runs as bash).
- Enter mid-turn now **queues** the input (claude-code parity) instead
  of steer-injecting into the running turn. The queue auto-drains when
  the current turn finishes; `Ctrl+C` clears it. Slash commands keep
  their existing mid-turn semantics. A queued-pill (`◷ queued × N: …`)
  appears above the input box when the queue is non-empty.
- Ranges single-line `↑`/`↓` jump to start / end of input (in addition
  to history recall when the input is empty).

#### UI

- Welcome banner redesigned: ASCII robot icon in a rounded blue
  border, model + mode + cwd, no session UUID. The compact strip
  (`✻ metis · model · mode · cwd`) is now sticky above the chat list,
  so the working directory stays visible during long agent runs.
- Bottom status bar version trimmed from
  `current: v0.1.3-21-gab7a825-dirty` to `current: v0.1.3` via a
  `shortSemver` helper. Full build fingerprint stays in `metis version -V`.
- Edit / Write tool results render unified diffs with red-bg `-` and
  green-bg `+` lines (word-level highlights for paired delete + insert).
  Tested via `internal/tui/render_diff_test.go` asserting SGR codes.

#### Tests

- `scripts/e2e/tmux_drive.sh` — full-coverage e2e harness using
  `tmux send-keys` + `capture-pane`. Replaces the macOS osascript
  driver for headless / CI runs. 8 cases ship today (banner_renders,
  slash_help, input_repeat, arrow_jump, slash_skills, slash_mcp_list,
  double_esc_clear, ctrl_c_quit).

## [0.1.1] - 2026-05-01

### Added

- `metis update` subcommand — self-update against the private GitHub release
  (atomic replace, sha256-verified). Refuses to clobber a `go install`-managed
  binary.
- Daily startup notice when a newer release is available (terminal only;
  throttled to one network call per 24h; `METIS_NO_UPDATE_CHECK=1` disables).
- Cross-compile release workflow (`.github/workflows/release.yml`): tag push
  or manual dispatch builds darwin/linux × amd64/arm64 tarballs and uploads
  them with sha256 sidecars.
- `install/install.sh` rewritten for the private-release token model — set
  `METIS_GITHUB_TOKEN` (fine-grained PAT, Contents: Read-only) and
  `curl … | bash` works.

### Fixed

- `internal/tui` bridge startup race: `startBridge` now waits for `/health`
  to respond before returning, eliminating EOF flakes on slower runners.

## [0.1.0] - 2026-04-30

### Added

- Initial public release.
- Agent loop: streaming message → tool → message cycle with cancel support.
- 16 built-in tools: Bash, Read, Edit, Write, Glob, LS, Grep, WebFetch,
  WebSearch, TaskCreate / TaskList / TaskUpdate, NotebookEdit, ExitPlanMode,
  AskUserQuestion, Agent.
- Multi-provider LLM support: Anthropic, OpenAI, Gemini, custom OpenAI-compatible.
- Memory system: Core / Archival / Recall + Daily journal.
- Bubbletea TUI chat surface with permission prompts (allow / deny / ask).
- Plugin and skill registry under `pkg/`.
- Agent Client Protocol (ACP) JSON-RPC server in `acp/`.
- Compaction: automatic conversation compression with configurable triggers.
- Config: `~/.metis/config.toml` with `api_key_env` for keeping secrets out of
  the file.

[Unreleased]: https://github.com/Ricardo-M-L/metis/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/Ricardo-M-L/metis/releases/tag/v0.1.1
[0.1.0]: https://github.com/Ricardo-M-L/metis/releases/tag/v0.1.0
