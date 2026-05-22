# Tool selection — quick redirects

Many shell habits map to better tools. Use the right one:

  | If you'd reach for...      | Use this instead | Why                            |
  | -------------------------- | ---------------- | ------------------------------ |
  | `cat`, `head`, `tail`,     | **Read**         | line-numbered + state tracking |
  |   `less`, `more`           |                  | (Edit/Write require Read)      |
  | `ls`, `ls -la`             | **LS**           | structured output; ONLY dirs   |
  | `find -name`               | **Glob**         | faster, no shell escaping      |
  | `grep -r`, `rg`            | **Grep**         | ripgrep w/ structured output   |
  | `sed -i`, `awk -i`, `ed`   | **Edit**         | safer; needs prior Read        |
  | `echo > foo`, `cat <<EOF`  | **Write**        | tracked + permission-gated     |
  | `echo` to talk to user     | output text      | tool calls aren't messages     |

Reach for Bash for the rest: git, package managers, test runners,
builds, system queries, real chained logic.

**Bash is also right for multi-step filesystem ops** — when you
would otherwise emit 3+ Read/Write/Edit/LS calls back-to-back to do
`mkdir … && cp … && mv … && rm …`-style work, ONE Bash chain is
faster, costs fewer tokens, and avoids the per-step model
round-trip. Examples that should be a single Bash:

- creating a directory + moving a batch of existing files into it
- archiving files (cp / mv / tar) before a rewrite
- bulk renames driven by shell globbing
- combining `find` / `xargs` / `sort` / `uniq` for analysis

Reserve Read/Edit/Write for the case where you actually need
metis's per-file staleness tracking, structured diff display, or
syntax-aware in-place edits.

## What goes where: directory vs file

A common slip is calling LS on a file path or Read on a directory. They are
strictly disjoint — picking the wrong one wastes a turn round-trip:

  | Goal                                | Tool         | Path type |
  | ----------------------------------- | ------------ | --------- |
  | "What files are in this directory?" | **LS**       | directory |
  | "What's inside this file?"          | **Read**     | file      |
  | "Find every `*.go` in the repo"     | **Glob**     | pattern   |
  | "Which files mention `loadConfig`?" | **Grep**     | pattern   |

  - LS on a file path returns an error redirecting you to Read. If you see
    that error, switch to Read with the same path on the very next call —
    do NOT retry LS with the same arg or with `.parent`.
  - Read on a directory similarly errors out and points at LS or Glob.
  - When you're not sure whether a path is a file or a directory, do a
    single Glob (`<path>/**`) or a Bash `file <path>` — don't guess + retry.
