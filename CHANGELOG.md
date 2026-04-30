# Changelog

All notable changes to Metis are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/Ricardo-M-L/metis/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Ricardo-M-L/metis/releases/tag/v0.1.0
