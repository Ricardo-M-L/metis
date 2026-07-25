# internal/tools/builtin

All built-in tools the agent can call: file I/O (Read/Write/Edit),
shell (Bash + the job pool), search (Glob/Grep/Regex), web
(WebFetch/WebSearch/web_browse), agent dispatch (Agent/Fork/Task),
memory, MCP-info, etc. Each tool is one or a few files implementing
the `pkg/tool.Tool` interface; the registry in `register.go` wires
them into `internal/runtime/tools.go`.

## File-naming convention (browse by family)

| Family | Files | What it owns | Entry file |
|---|---|---|---|
| `bash*` | 7 | `bash.go` core + `bash_jobs.go` (background pool) + `bash_args_blocker.go` + `bash_env.go` + `bash_policy.go` (sandbox allow/deny lists) + `bash_security_rules.go` (30+ adversarial-input rules) | `bash.go` |
| `agent.go` + `agent_*` + `subagent_jobs.go` + `task.go` + `todo.go` + `fork.go` | ~6 | Agent / Fork / Task / TodoWrite / Subagent-job dispatcher (these are the multi-agent fan-out tools) | `agent.go` (entry) |
| `web*` | 3 | `webfetch.go` (URL → text/binary), `websearch.go` (search engine), `web_browse.go` (interactive multi-step browsing) | by name |
| `read*` + `write.go` + `edit.go` + `notebook.go` | 4 | File I/O surface | `read.go`, `write.go`, `edit.go` |
| `glob.go` + `grep.go` + `regex.go` + `ls.go` | 4 | Filesystem search | by name |
| singletons | ~10 | `ask.go` (AskUser), `monitor.go` + `wakeup.go` (background tasks), `lsp.go`, `git.go`, `skill.go`, `memory.go`, `metis_info.go`, `plan_mode.go`, `message_teammate.go`, `sendmessage.go` |
| infra | `register.go` (assembles tool slice), `util.go`, `scope.go`, `read_state.go`, `capabilities.go`, `classifier.go` |

## Where do I find...

- **Bash + security rules** → `bash.go`, `bash_security_rules.go` (30+ rules numbered to claude-code's bashSecurity.ts), `bash_args_blocker.go`
- **Background job pool** → `bash_jobs.go` (BashList/BashOutput/BashKill use it), `subagent_jobs.go`
- **Agent / Fork / Task** (multi-agent fan-out) → `agent.go` (Description has the contract spec), `fork.go`, `task.go`
- **WebFetch binary handling** (auto-save image/PDF to disk instead of inlining bytes) → `webfetch.go`
- **AskUser tool** → `ask.go`
- **Background-task monitor / scheduled wakeup** → `monitor.go`, `wakeup.go`
- **Skill execution** (one-shot skills compiled into Tool form) → `skill.go`
- **Tool registration** (the slice the runtime imports) → `register.go`

## Design invariants

- Each tool implements `pkg/tool.Tool`: `Spec()` for the schema,
  `CanUse(ctx, input)` for permission gate input, `Execute(ctx, input)`
  for the work.
- `Bash.CanUse` runs `CheckCommand(cmd)` (bash_security_rules.go) BEFORE
  the user-permission gate. Even in `--mode bypassPermissions`, security rules
  still block — these are adversarial-input checks, not user
  preferences.
- Tools that produce binary output (audio/video/image/PDF) save to
  `~/.metis/tool-results/<ts>.<ext>` and return a one-line pointer,
  NOT the bytes. See `webfetch.go::saveBinaryResponse`.
- Tools that need to mutate model-visible Model state (e.g., AskUser)
  do so via `pkg/tool.Result` fields, not direct Model access.
