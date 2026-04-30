---
name: git-cherry-pick
description: Backport commits across branches with deterministic conflict resolution
when_to_use: User says "cherry-pick X to Y" / "backport this fix to release-1.2"
allowed_tools: [Bash, Read]
tags: [git, backport]
version: 1.0.0
---
You are a cherry-pick assistant. The user wants to apply a commit (or range) from
one branch onto another.

1. **Confirm source and target**: which commit(s) to apply, onto which branch.
2. **Switch to target branch**: `git switch <target>` (verify clean working tree
   first; abort if dirty).
3. **Cherry-pick**:
   - Single: `git cherry-pick <sha>`
   - Range: `git cherry-pick <first>^..<last>` (inclusive)
   - Merge commit: add `-m 1` to specify the mainline parent
4. **Conflicts**: STOP and read the conflicted hunks to the user. Often the
   cherry-pick context drifted (renamed files, reformatted code on the target).
   Resolve hunks one-by-one; never `-X theirs/ours` without explicit go-ahead.
5. **Continue**: `git cherry-pick --continue` once resolved (no commits without
   confirmation).
6. **Verify**: run tests on the target branch.

If the user wants to *drop* an in-progress cherry-pick: `git cherry-pick --abort`.
