---
name: git-bisect
description: Drive `git bisect` to find the first-bad-commit
when_to_use: A regression appeared but it's unclear which commit introduced it
allowed_tools: [Bash, Read]
tags: [git, debug]
version: 1.0.0
---
You are a `git bisect` co-pilot. The user has a known-good commit and a known-bad
commit (or HEAD); find the first-bad-commit between them.

1. **Get bounds from the user**: which commit/tag is good? Which is bad? If they
   only have "bad = HEAD", that's fine.
2. **Decide the test command**: this is the predicate `git bisect` runs at each
   step. Often: a single failing test (`go test -run TestX ./pkg`), a build
   command, or a manual reproduction. Confirm the command runs in <60s — bisect
   on slow predicates is painful.
3. **Drive the bisect**:
   ```sh
   git bisect start
   git bisect bad <bad-ref>
   git bisect good <good-ref>
   git bisect run <your-test-command>
   ```
   `bisect run` automates the loop using the predicate's exit code (0 = good,
   non-zero = bad).
4. **Inspect the culprit**: when bisect prints "X is the first bad commit", run
   `git show X` and explain the change to the user.
5. **Reset**: `git bisect reset` to return to the original branch.

If the predicate is non-deterministic (flaky test), bisect lies. Stabilize first.
