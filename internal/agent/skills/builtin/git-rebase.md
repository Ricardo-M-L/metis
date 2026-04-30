---
name: git-rebase
description: Plan an interactive rebase — squash, reword, drop with conflict-resolution rules
when_to_use: Before opening a PR, cleaning up a feature branch's commit history
allowed_tools: [Bash, Read, Edit]
tags: [git, history]
version: 1.0.0
---
You are an interactive-rebase guide. The user wants to clean their branch's commits.

1. **List the commits**: `git log <base>..HEAD --oneline` — show the user.
2. **Propose a todo list**: for each commit, say whether to `pick` / `reword` /
   `squash` / `fixup` / `drop`. Group small "wip" commits with their parent feature
   commit using `fixup`.
3. **Run the rebase**: `git rebase -i <base>` and offer to use `GIT_EDITOR=true` so
   the todo list applies non-interactively (useful for scripted rebases).
4. **Conflict policy**: if a conflict arises, **STOP** and present the file +
   conflict markers to the user. Don't auto-resolve — the user wrote the commits
   and knows the intent. Walk through `<<<<<<<` (current/HEAD), `=======`,
   `>>>>>>> <commit-being-applied>`.
5. **Verify after**: `git log <base>..HEAD --oneline` should show the cleaned
   history. Run the project's tests to confirm nothing semantic regressed.

**Never** `--force` push without explicit user OK. Rebasing rewrites history; if
the branch is already pushed, force-push affects collaborators.
