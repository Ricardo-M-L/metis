<!-- Thanks for the patch. Please answer all three sections. -->

## What

<!-- One short paragraph: what does this change? -->

## Why

<!-- The motivation. Link related issue with `Fixes #N` / `Refs #N`. -->

## How was it tested

Root module (required):

- [ ] `gofmt -l .` returns nothing
- [ ] `go vet ./...` is clean
- [ ] `go build ./...` succeeds
- [ ] `go test -count=1 -timeout 90s ./...` passes locally

Additional CI surfaces:

- [ ] Patched Bubble Tea: `go vet ./...` and `go test -race -count=1 ./...`
      from `vendor-patches/bubbletea-v2`
- [ ] Patched Ultraviolet: `go vet ./...` and `go test -race -count=1 ./...`
      from `vendor-patches/ultraviolet`
- [ ] Desktop frontend: `npm ci`, `npm run check`, and `npm run build` from
      `metis-desktop/frontend`
- [ ] Desktop Go module: `go vet ./...`, `go test -race -count=1 ./...`, and
      `go build -tags production ./...` from `metis-desktop`
- [ ] GitHub Actions is green on Ubuntu, macOS, and Windows
- [ ] New behavior is covered by a test, OR manually tested (describe below)

<!-- If manually tested, paste the commands you ran and the output. -->

## Checklist

- [ ] Single concern (refactor and feature are separate PRs)
- [ ] Title follows commit style (imperative, ≤70 chars)
- [ ] Public API changes (under `pkg/`) are documented
- [ ] No secrets, large binaries, or generated files committed
