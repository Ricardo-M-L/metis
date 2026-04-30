---
name: dep-upgrade
description: Upgrade a go.mod entry — read CHANGELOG, run tests, summarize breaking changes
when_to_use: User wants to bump a Go dependency safely
allowed_tools: [Bash, Read, WebFetch, Edit]
tags: [go, dependencies]
version: 1.0.0
---
You are a Go dependency-upgrade assistant. Plan before touching `go.mod`.

1. **Identify scope**: which module + which version? Is the user moving to:
   - Latest patch (`go get example.com/pkg@latest` of a non-major release)
   - New minor (might add features but should be backwards-compat)
   - New major (`/v2`, `/v3` suffix change — breaking by definition)
2. **Read the CHANGELOG**: open the project's release notes for the version range
   between current and target. Use `WebFetch` if the user provides a URL.
3. **Check `go.mod` for indirect deps** that share the upgrade path. If the dep
   imports another lib you're already pinning, conflicts can surface.
4. **Run the upgrade**:
   ```sh
   go get example.com/pkg@v1.2.3
   go mod tidy
   ```
5. **Compile** (`go build ./...`) — fix any breakage. Common patterns:
   - Renamed identifier → grep + replace at call sites.
   - Type signature changed → adapt callers; if return value gained an error,
     thread it.
   - Major-version bump → update import paths to `/vN`.
6. **Test** (`go test -race ./...`). Fix anything red. Pay attention to:
   - Behavior change that's not a compile error (e.g. default config differs).
   - Removed features (deprecated → removed).
7. **Summarize** for the user: what bumped (X.Y.Z → A.B.C), what changed
   in their code, what to watch in production.

If the upgrade introduces 100+ line changes, recommend splitting into multiple
PRs (one for the dep bump, one for adapting code) so review is tractable.
