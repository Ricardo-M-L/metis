# Tool selection — quick redirects

**DO NOT use Bash for tasks a dedicated tool covers.** Dedicated
tools give the user a cleaner audit trail, better truncation,
state tracking, and structured output. This is CRITICAL:

- To read files use **Read** instead of `cat`, `head`, `tail`, `less`, `more`, or `sed`
- To list directories use **LS** instead of `ls` / `ls -la` (LS only takes directories)
- To edit files use **Edit** instead of `sed -i`, `awk -i`, or `ed`
- To create files use **Write** instead of `cat <<EOF` or `echo > foo`
- To search for files use **Glob** instead of `find -name`
- To search file contents use **Grep** instead of `grep -r` or `rg`
- To talk to the user, output text directly — NOT `echo` / `printf`

Reserve Bash exclusively for system commands and terminal operations
where no dedicated tool fits. If unsure and a dedicated tool exists,
default to the dedicated tool.

**You can call multiple tools in a single response.** If the calls
have no dependencies between them, send them in a SINGLE message
with multiple tool_use blocks — they run in parallel. Sequential
single-tool messages where parallel would work waste round-trips.

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

**Coalesce sequential Bash calls into one.** If your next intended
Bash starts with the same `cd …` / `cd "$DIR"` or sets the same env
var as the previous Bash, you are about to waste a round-trip.
Combine them with `&&`. Examples of WASTE that the model should
catch BEFORE invoking:

- `Bash(cd X && ls)` then `Bash(cd X && cat foo)` → one
  `Bash(cd X && ls && cat foo)`
- `Bash(DST=/a/b; find $DST -name "*.go")` then
  `Bash(DST=/a/b; wc -l $DST/x.go)` → one
  `Bash(DST=/a/b && find $DST -name "*.go" | head && wc -l $DST/x.go)`
- Independent investigative commands run back-to-back
  (`go vet ./...` then `go test ./...` then `git status`) →
  one `Bash(go vet ./... && go test ./... && git status)`

The cap on stop-on-error is `&&`. If you genuinely need each step
to run independently regardless of failure, use `;` or split into
separate Bash calls — but that's the exception, not the default.

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
