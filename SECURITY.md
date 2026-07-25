# Security Policy

A Chinese version is available at [SECURITY.zh-CN.md](SECURITY.zh-CN.md).

## Supported versions

Metis is on `0.x`. Only the latest minor release receives security fixes.

| Version | Supported |
|--------:|:---------:|
|   0.1.x |    ✅      |

## Reporting a vulnerability

**Do not open a public GitHub issue for security problems.**

Email the maintainer at `Ricardo-M-L@users.noreply.github.com` with:

- A description of the issue
- Steps to reproduce (PoC code or commands welcome)
- Affected versions
- Any known mitigations

You should expect:

- An acknowledgement within **5 business days**
- A fix or status update within **30 days** for confirmed issues
- Coordinated disclosure: we will agree on a release date before any details
  are made public, and credit the reporter unless they prefer anonymity

## Threat model — what's in scope

In scope (we want to know):

- Sandbox / permission-gate bypass: a tool call running outside what the
  permission mode advertised
- Path traversal in Read / Edit / Glob / LS
- Command injection in Bash policy
- Plugin or MCP-server escalation that lets a third-party server read
  credentials, system files, or other plugins' state
- Memory store leaking secrets across users / sessions on a shared host

Out of scope (please don't report):

- The user voluntarily setting `--mode bypassPermissions` and running unsafe commands —
  that mode is documented as "you are on your own"
- Issues in upstream LLM provider APIs
- Denial of service via giant inputs (Metis is a local CLI; use ulimit)
- Sensitive data the user themselves wrote to a config file in plain text

## Hardening tips

- Never pass real API keys via the `api_key = "..."` field in config.toml.
  Use `api_key_env = "ANTHROPIC_API_KEY"` and keep the secret out of the file.
- Default to `mode = "default"` until you've reviewed which tools you trust to
  auto-run.
- Treat `~/.metis/` as you would treat `~/.ssh/` — same filesystem permissions,
  no checking it into a shared dotfiles repo.
