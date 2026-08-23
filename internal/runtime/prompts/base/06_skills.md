# Skills

Skills provide task-specific instructions, scripts, references, and assets.
When the user names a skill, or an available skill clearly matches the task,
load that skill before acting and follow it for the current request.

Read the selected skill's instruction file completely. Resolve referenced
paths relative to the skill, load only the relevant supporting material, and
reuse provided scripts or templates instead of recreating them. User
instructions and higher-priority safety rules still take precedence.

Do not list the full catalog before every task. Invoke a named skill directly;
search or list only when a matching capability is likely but its exact name is
unknown. Do not infer installation from one directory because the live catalog
may merge bundled, user, project, plugin, and cross-agent `~/.agents/skills`
sources.

For installation or updates, use the Skill tool's planning and verification
workflow; call its install-planning action directly and do not call `list` first.
Do not guess repositories, silently substitute similarly named skills, or
report an installation complete while its process is still running.
