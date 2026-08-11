# internal/tools/builtin

Built-in implementations of the model-callable tool contract. The package
covers local files, shell and jobs, search, web and images, multi-agent work,
tasks, plan transitions, interaction, memory/history/skills/workflows, MCP
resources, channels, and introspection. This README names stable families and
assembly points rather than maintaining a fixed tool inventory; the live set
depends on configuration and runtime services.

## Browse by concern

| Concern | Main files |
|---|---|
| Shell, command classification, sandboxing, and background jobs | `bash/` (`bash.go`, `jobs.go`, `policy.go`, `security_rules.go`, `args_blocker.go`, `sandbox_*.go`) |
| Local files and search | `read.go`, `write.go`, `edit.go`, `notebook.go`, `ls.go`, `glob.go`, `grep.go`; `regex.go` is a helper, not a standalone tool |
| Web and multimodal results | `webfetch.go`, `websearch.go`, `web_browse.go`, `view_image.go` |
| Agents and collaboration | `agent.go`, `agent_permission_boundary.go`, `fork.go`, `subagent_jobs.go`, `message_teammate.go`, `sendmessage.go` |
| Tasks and planning | `task.go`, `todo.go`, `plan_mode.go` |
| Human/background coordination | `ask.go`, `monitor.go`, `wakeup.go`, `cron_tools.go` |
| Knowledge and automation | `skill.go`, `memory.go`, `history.go`, `workflow.go`, `slash_command.go` |
| MCP and environment inspection | `mcp_resources.go`, `metis_info.go`, `lsp*.go`, `git.go` |
| Shared infrastructure | `register.go`, `capabilities.go`, `runtime_rebind.go`, `scope.go`, `read_state.go`, `util.go` |

## Registration and availability

The available registry is assembled in two stages:

1. `register.go` installs dependency-light base tools and filters configured
   disables plus each tool's `IsEnabled()` result.
2. `internal/runtime/tools.go` adds tools that need live provider, roster,
   jobs, monitor, cron, channel, session, plugin/skill, memory, workflow, or
   plan dependencies.

Consequently, neither `register.go` nor this directory's filenames alone are
the authoritative tool list. Inspect `BuildToolRegistry` and the final
`tools.Registry` for the current command/session.

## Tool contract and capabilities

Every tool implements `pkg/tool.Tool`:

```go
Name() string
Description() string
InputSchema() map[string]any
Concurrency(input map[string]any) tool.Concurrency
CanUse(ctx context.Context, input map[string]any) (tool.Permission, string)
Execute(ctx context.Context, input map[string]any) (*tool.Result, error)
IsEnabled() bool
```

`Concurrency` is input-dependent. In one streamed batch:

- `Safe` calls fan out concurrently.
- `Queue` calls serialize FIFO with one another while safe calls continue.
- `Background` calls return a job handshake while their owning job system
  manages later completion.
- `Exclusive` calls preserve order and wait until safe/queued work finishes.

For example, Bash classifies known read-only commands as `Safe` and fails
closed to `Exclusive`; WebBrowse is `Queue`; foreground Agent is `Exclusive`
and `run_in_background:true` is `Background`. The dispatcher, not an
individual tool, enforces cross-tool ordering; see `internal/agent/dispatch.go`.

Optional capability interfaces in `pkg/tool` report per-input read-only or
destructive behavior, mandatory human interaction, bypass immunity,
interrupt policy, and textual result spill thresholds. Their defaults are not
one blanket safety posture: missing read-only, destructive,
interaction-required, and bypass-immune capabilities all report `false`,
while missing interrupt policy defaults to `Cancel`. Side-effecting tools such
as Bash/Edit/Write/SendMessage opt into blocking interruption so they cannot
be left half-applied.

## Permission and execution boundaries

`CanUse` must be a cheap synchronous decision; expensive work belongs in
`Execute`. Bash is stricter than the general gate:

1. `CheckCommand` applies the numbered adversarial-input rules first.
2. Only a passing command reaches the user permission gate.
3. Sandbox auto-allow upgrades any `Ask` decision, but never a `Deny`.
   Plan mode, `dontAsk`, and explicit deny rules therefore remain hard stops.
4. When auto-allow is active, the OS sandbox becomes the approval boundary
   for calls that otherwise would have prompted, including safety-path asks.

Therefore `bypassPermissions` cannot override Bash's hard security rules.
Avoid documenting a fixed rule count; the rule IDs and adversarial tests in
`bash/security_rules*.go` are the source of truth.

## Results, images, and interaction

`pkg/tool.Result` carries canonical text in `Output`, optional display/meta
data, and optional base64 `Images` attachments. Binary handling is
tool-specific:

- WebFetch saves recognized binary responses under
  `~/.metis/tool-results/` and returns a textual pointer.
- ViewImage returns a base64 image attachment; dispatch converts it to the
  provider's native image block when vision is supported.

Large textual results may be spilled by the agent dispatcher according to
`MaxResultSizeChars`; a tool may override or disable that threshold.

AskUser does not mutate the TUI `Model` through special result fields. It
publishes an `EventAskUser` carrying a reply channel, waits for the UI/user
response, and returns that answer as an ordinary tool result. The TUI remains
the owner of its own model state.
