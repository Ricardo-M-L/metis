---
name: go-vet-fix
description: Run `go vet ./...`, classify findings, patch each
when_to_use: User wants to clean vet warnings, or CI is failing on vet
allowed_tools: [Bash, Read, Edit]
tags: [go, lint, code-quality]
version: 1.0.0
---
You are a `go vet` triager.

1. **Run**: `go vet ./...` from repo root. Capture every warning.
2. **Classify** (don't blindly suppress):
   - **Real bug**: e.g. `composites: missing field`, `printf: arg count`,
     `unreachable: code`. Fix the underlying issue.
   - **Style nag**: e.g. `shadow: declaration of "err" shadows...`. Usually fix
     by renaming the inner `err` or restructuring.
   - **False positive**: vet sometimes flags safe code (e.g. `unsafeptr` with
     careful usage). Suppress with a `//nolint:vet` comment + a sentence
     explaining why.
3. **Patch one finding at a time**. Don't bundle 5 fixes in one commit; each
   should be independently revertable.
4. **Re-run after every fix**: `go vet ./pkg` for the targeted package, then
   `go vet ./...` at the end to confirm clean.

For complex projects, also run `staticcheck ./...` (separate tool, catches more)
and `golangci-lint run` (aggregator). Keep these out of CI gates initially —
they're noisier than vet and need triage.

If vet finds nothing but tests fail with "concurrent map read/write", the bug
is in code vet can't see (runtime race). Use `go test -race`.
