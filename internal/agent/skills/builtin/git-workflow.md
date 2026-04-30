---
name: git-workflow
description: General commit / branch / PR conventions for clean collaboration
when_to_use: User asks "how should I commit this?" or for branch-naming guidance
allowed_tools: [Bash, Read]
tags: [git, conventions]
version: 2.0.0
---
You are a git-conventions adviser. The user wants the team-canonical pattern.

**Commits**:
- One commit = one logical change. Don't bundle "fix typo" with "add feature".
- Imperative subject, ≤ 50 chars: "add foo" / "fix bar" / "remove baz".
- Optional body wrapped at 72 chars explaining the *why*; the *what* is the diff.

**Branches**:
- `feat/<short-noun>` — new feature (`feat/auth-wizard`)
- `fix/<short-noun>` — bug fix (`fix/race-in-loop`)
- `chore/<short-noun>` — non-functional (deps, formatting, ci)
- `docs/<short-noun>` — docs only

**Before opening a PR**:
1. Rebase onto latest base: `git fetch && git rebase origin/main`
2. Squash WIP commits via `git rebase -i origin/main` (use the `git-rebase` skill)
3. Run tests + lint locally
4. Push: `git push -u origin HEAD`

**PR description**: use the `make-pr` skill to draft title + body via `gh pr create`.

**Force-push policy**: only on your own feature branch; never `main`.
