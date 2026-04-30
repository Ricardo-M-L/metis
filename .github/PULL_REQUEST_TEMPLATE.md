<!-- Thanks for the patch. Please answer all three sections. -->

## What

<!-- One short paragraph: what does this change? -->

## Why

<!-- The motivation. Link related issue with `Fixes #N` / `Refs #N`. -->

## How was it tested

- [ ] `go test -count=1 -timeout 90s ./...` passes locally
- [ ] `go vet ./...` is clean
- [ ] `gofmt -l .` returns nothing
- [ ] New behavior covered by a test, OR manually tested (describe below)

<!-- If manually tested, paste the commands you ran and the output. -->

## Checklist

- [ ] Single concern (refactor and feature are separate PRs)
- [ ] Title follows commit style (imperative, ≤70 chars)
- [ ] Public API changes (under `pkg/`) are documented
- [ ] No secrets, large binaries, or generated files committed
