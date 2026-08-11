# internal/agent/skills

The skill catalog and lifecycle implementation. A filesystem skill is
normally a Markdown file with optional YAML frontmatter; its Markdown body
becomes the prompt returned to the model. Legacy and marketplace manifests
may be JSON. The LLM-facing entry point is the `Skill` tool, whose schema
requires an action, for example `Skill(action="invoke", name="debug")`.

## Browse by concern

| File | What it owns |
|---|---|
| `loader.go` | Multi-source `Loader`, layer priority/trust stamping, activation/prerequisite filtering, caching, and collision resolution |
| `manifest.go` | Markdown frontmatter and legacy JSON parsing; the canonical manifest type is `pkg/skill.Skill` |
| `embedded.go` + `builtin/` | Skills compiled into the binary |
| `store.go` | Local on-disk JSON catalog with list/get/save/delete and use-count updates; it is not the live multi-source loader |
| `marketplace.go` | Registry/source fetch and install plumbing used by `metis skills install` |
| `search.go` | GitHub code search for marketplace-style JSON manifests, not local fuzzy lookup |
| `safety.go` | Trust-aware prompt-injection and exfiltration pattern scan over `Prompt` and `WhenToUse` |
| `inline_shell*.go` | Trusted-skill `!` shell expansion, timeout/output caps, process cancellation, and sandbox integration |
| `expand.go` | `${METIS_SKILL_DIR}` / `${METIS_SESSION_ID}` template variables and compatible aliases |
| `filter.go` | Shared junk-filename filter for AppleDouble files, editor backups, and `.DS_Store` |
| `usage.go`, `curator.go`, `overlap.go` | Usage history, recoverable stale-skill archiving, and cheap deterministic overlap detection |
| `synth.go`, `synth_tool.go` | Dream-cycle skill synthesis helpers; `SkillSynth` is deliberately scoped to the dream registry rather than the normal global tool registry |

## Manifest schema

`pkg/skill.Skill` is the source of truth. Its serialized schema includes
`name`, `description`, `category`, `prompt`, `tools`, `tags`, `created_at`,
`uses`, `allowed_tools`, `when_to_use`, `dont_use_when`, `user_only`,
`disabled`, `activate_on`, `model_override`, `version`, `content_hash`,
`source`, `prerequisites`, and `trust_level`. For Markdown skills the body
supplies `prompt`. `trust_level` is stamped by the loader and is not
authoritative when supplied by a manifest; `local_path` is runtime-only and
is not serialized.

`permission_mode`, `effort`, and `max_turns` are sub-agent profile fields,
not skill fields. The manifest parser does not turn a skill into a separate
permission scope.

The `Skill` tool supports `list`, `get`, `invoke`, and `plan_install`.
`list` and `get` are classified read-only for permission and plan-mode
purposes. Outside plan mode, `get` may still persist view-usage metadata; it
does not alter the skill content. `invoke` goes through the normal permission
gate because it records usage and a trusted skill may run inline shell while
expanding its body. Plan mode permits inspection but rejects invocation.

## Sources, priority, and trust

The conventional priority order in `loader.go` is:

```text
bundled(0) < optional(5) < universal(8) < user(10)
  < project(20) < plugin(30) < mcp(40)
```

The standard runtime currently wires layers through `plugin`; `mcp(40)` is a
reserved loader convention, not a promise that MCP-contributed skills are in
the live catalog. Layers are scanned in ascending priority and a later,
higher-priority skill with the same name replaces the earlier one. Do not
rely on the winner for collisions at the same priority: no source-path
tie-break is implemented. The visible catalog is sorted by skill name after
resolution.

Every standard runtime layer supplies a non-empty trust value, which overrides
a manifest's claimed trust. A custom `Layer` with empty `Trust` does not
override the manifest, so extension authors must set it explicitly.
`ScanSkill` rejects obvious prompt-injection, secret-exfiltration,
role-override, and shell-escape patterns for scanned trust classes; bundled
and project skills are exempt in the current policy. Inline shell runs only
for `builtin`, `trusted`, `user`, or `project` trust, and uses the shared
sandbox when one is configured. Community and unknown-trust skills never
execute inline shell.

## Important boundary: `allowed_tools`

Despite its name and the comment on `pkg/skill.Skill.AllowedTools`, the
current `Skill` tool only appends `allowed_tools` to the model-facing skill
text. It does not narrow the runtime tool registry or permission gate.
Treat it as advisory metadata, not a security boundary, until invocation is
wired to an enforced per-skill registry.

`tools` is likewise rendered as a suggested-tools hint when
`allowed_tools` is empty.

## Lifecycle

- Outside plan mode, `Skill(action="get", ...)` records a view for a
  user-owned skill; `Skill(action="invoke", ...)` records a use.
- Dream synthesis records create/patch provenance. It is not exposed as a
  `metis skills synth` CLI subcommand.
- `curator.go` treats flat `.md`/`.markdown` files directly under the user
  skill root as eligible and archives stale candidates recoverably. Directory
  installs, JSON files, symlinks, dotfiles, bundled/project skills, and pinned
  skills are outside that pruning path. Flat-file eligibility is a provenance
  heuristic; the curator cannot distinguish a hand-authored flat file from a
  synthesized one, so users should pin files that must never age out.
- `overlap.go` nominates near-duplicate clusters before the more expensive
  dream consolidation pass.
