# Session files and input trace

Desktop/Web chat has a **文件** (Files) entry. Click a known local file link,
inline-code path, or file chip to preview it on the right without replacing
the conversation. Markdown has rendered/source views; other supported UTF-8
text and code files use a line-numbered source view with offline syntax
highlighting. Open files have separate, closable tabs (up to 12); each retains
its source/preview mode, line-wrap preference and scroll position. Use **+**
to search the session's known files, drag the divider (or use its arrow keys)
to resize, maximize for focused reading, or collapse to return to chat.
Refresh re-reads the selected file from disk; Copy uses the original source
buffer, not rendered markup or line numbers. Narrow windows use a closable overlay.
This is separate from versioned HTML Artifacts. HTML files here are shown as
source, never executed. PDF/image rendering is not part of this text preview.

CLI uses the same discovery and read-only validation:

```sh
metis files list --session SESSION_ID
metis files show FILE_ID --session SESSION_ID
```

Both accept `--json`. Default storage is `METIS_HOME/sessions` (normally
`~/.metis/sessions`); pass `--sessions-dir PATH` for a custom session store.
The commands do not invoke a model, load credentials, or modify the session.

Files are discovered from successful Write/Edit/apply_patch results and
explicit assistant links to supported local text files. Bare prose is not
an authorization to read a path. Link-only files must be inside the session
workspace or a system temporary directory. Every content request revalidates
the session-scoped ID, resolved path and open file identity. Credential paths
are excluded, content is redacted, terminal controls are stripped, and text
previews are capped at 512 KiB. Deleted files are not recoverable via preview.
Syntax parsing falls back to plain text for large/long-line inputs; the source
viewer renders at most 12,000 lines and explicitly marks a truncated display.
Tab caches are in-memory only and are cleared when changing sessions. Read-only
preview never executes scripts, creates remote images, or grants filesystem access.
Only references still present in saved history can be rediscovered.

Human submissions in CLI/TUI/REPL/Desktop are recorded as USER, including safe
attachment placeholders. Automatic notifications and cron input use CONTEXT
with a source category, rather than pretending to be human input. Context
events contain the display-safe injected content, not just a completion count.

For legacy partial traces, the UI can recover missing USER rows only when
saved history and trace anchors agree. It preserves original trace events,
turns and metrics, and marks recovered rows `history-reconstructed`; missing
original timestamps remain unknown. Ambiguous history is not guessed. Legacy
plain-text automatic input without provenance cannot always be distinguished
from a human message.
