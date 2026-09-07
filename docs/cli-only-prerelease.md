# CLI-only release protocol

`v0.4.52` is prepared as a **formal CLI-only release** with GitHub `prerelease=false`, not a
signed Desktop release. Its plain `vX.Y.Z` tag works with existing pinned
installers. Explicit `make_latest=false` keeps `/releases/latest` on the existing
full CLI+Desktop stable release, so the Desktop updater does not offer
Desktop users a CLI-only payload. CLI stable discovery is independent of that
pointer; see [the CLI channel and migration contract](cli-stable-channel.md).
Updating the five source version declarations
does not publish Desktop. This document keeps its original filename for links.

## Registered contract

- `.github/cli-only-releases.json` is the repository-reviewed allowlist. Only an
  exact registered tag may use the CLI-only path; a workflow input cannot opt an
  arbitrary stable tag into it. Both trusted tooling and tagged source must agree.
- Registry channel `cli-only-stable` requires `prerelease=false` (the v0.4.52
  setting); `cli-only-prerelease` requires `prerelease=true`. Both must not be
  the response from GitHub's independent `/releases/latest` endpoint and require **12**
  assets: CLI archives for Darwin/Linux/Windows × amd64/arm64 and six SHA-256 files.
- Publication explicitly sends `make_latest=false`. GitHub does not expose that
  field on release GET responses, so verification separately checks latest before
  and after publication. Never infer this setting from a release title/body.
- Unregistered releases retain the **20-asset stable contract** and the existing
  Developer ID, notarization, stapling, and Gatekeeper verification. CLI-only
  registration does not waive those requirements for full CLI+Desktop releases.
- Only drafts may be rebuilt/replaced. Once public, CLI-only releases are
  immutable; fixes need another version and an explicit registry review.
- The publication input must identify a successful `Release` workflow run from
  this same repository with a head SHA exactly equal to the tag commit. The
  verifier checks workflow path/ID, completed/success state, event, repository
  identity, artifact ID and source SHA, then compares every draft asset byte hash
  against that immutable workflow artifact before executing or publishing it.
  A different binary with the same version string and a fresh sidecar is rejected.
- Draft lookup uses GraphQL `repository.release(tagName:)`, resolves its numeric
  ID, then reads by that ID. Both the REST tag endpoint and complete REST lists
  can omit drafts for Actions tokens. Only an error-free GraphQL response for
  the exact visible repository with explicit `release: null` (lookup exit 4)
  permits creation; network/auth/JSON failures stop the workflow. `gh release
  view` must resolve the same ID before draft-aware upload/download commands run.
- Starting with v0.4.52, post-publication Linux and Windows checks exercise both
  pinned installation and an anonymous default installation with no version pin.
  An independent read-only API oracle snapshots the highest complete stable CLI
  before and after installation. The actual tag must be at least the release
  being checked and equal one of those two selected tags; an arbitrary newer
  or stale shared-latest version cannot pass. Both expected tags and the actual
  tag are recorded. Later higher CLI releases therefore do not break rechecks.
  Versions below v0.4.52 retain their historical pinned-only check, since their
  installers predate independent CLI discovery. Prerelease tags also skip this
  stable-channel gate.

## Owner-run release sequence

These are release-owner commands, **not a record that publication has happened**.
Do not tag, push, or dispatch until the source review and full CLI gates pass.
Use the reviewed complete source commit, not a dirty-tree evaluation candidate.

1. Confirm the exact new tag is absent and the stable latest tag is recorded:

   ```sh
   git ls-remote --tags origin refs/tags/v0.4.52
   gh api repos/Ricardo-M-L/metis/releases/latest --jq .tag_name
   scripts/verify-release-policy.sh
   scripts/verify-dist.sh --metadata-only --tag v0.4.52
   python3 -B -m unittest discover -s scripts -p 'test_release_contract.py' -v
   python3 -B -m unittest discover -s scripts -p 'test_verify_cli_default_channel.py' -v
   ```

2. After separately reviewing/committing all intended CLI fixes, publish the
   reviewed source and ordinary `v0.4.52` tag using the repository's owner process.
   The `Release` tag workflow runs root and patched-module tests, cross-builds
   all six CLI targets, checks archive shapes/checksums/version, and performs
   Linux and Windows artifact smoke tests. It stages a **draft only**, preserving
   the registry's `prerelease=false` for v0.4.52.
   It does not build or upload Desktop assets for this registered tag.

   To retry an existing tag whose release remains a draft:

   ```sh
   gh workflow run release.yml --ref v0.4.52 -f tag=v0.4.52
   gh run list --workflow release.yml --limit 5
   ```

   Re-running a build is supported, but is not a bit-for-bit reproducibility
   claim: the existing Makefile stamps build time and archives may have metadata.
   The public release bytes, once accepted, are immutable and SHA-256-addressed.
   Use the tag ref for CLI rebuilds: dispatching newer tooling from an unrelated
   main commit fails the exact head-SHA publication provenance gate.

3. Wait for the matching source/tag run to finish successfully, inspect its gates
   and the 12 draft assets, and review the public release notes for credentials,
   inaccurate six-hour claims, or stale Desktop instructions. Edit notes only
   while it is a draft. Then dispatch the bounded publication workflow:

   ```sh
   gh workflow run release-cli-publish.yml --ref main -f tag=v0.4.52 \
     -f build_run_id=REVIEWED_SUCCESSFUL_RELEASE_RUN_ID
   gh run list --workflow release-cli-publish.yml --limit 5
   ```

   This fails before writing if the release is already public, unregistered,
   on the full CLI+Desktop channel, latest, incomplete, has extra assets, has changed since download,
   lacks exact successful-build provenance, or fails checksum/archive/version
   validation. It publishes exactly once with
   the registered prerelease flag (`false` for v0.4.52) and `make_latest=false`, checks latest remained unchanged,
   then checks both pinned and default CLI installation on Linux and Windows.

4. Explicitly dispatch the read-only published check and inspect its final result:

   ```sh
   gh workflow run release-published.yml --ref main -f tag=v0.4.52
   gh run list --workflow release-published.yml --limit 5
   gh api repos/Ricardo-M-L/metis/releases/tags/v0.4.52 \
     --jq '{tag_name,draft,prerelease,assets:[.assets[].name]}'
   gh api repos/Ricardo-M-L/metis/releases/latest --jq .tag_name
   ```

   Do this even if a `release: published` event is configured: a write made with
   `GITHUB_TOKEN` may not trigger another workflow. Do not claim release success
   on dispatch alone. If a post-publish smoke fails, preserve the immutable
   release and report the failure; do not silently replace its assets.

## Install for the next independent evaluation

The user's selected local route is to build the published tag and install it at
the terminal's `metis` entry point. Record `command -v metis`, the binary's
version/commit and SHA-256, the resolved published tag commit, build command and
source cleanliness. Run the preflight and evaluation through that exact entry
point. Label this honestly as **local build from the published tag**, not the
downloaded GitHub asset; matching a version string alone is insufficient.

Downloading the public artifact is an optional separate distribution check.
The existing installer supports pinned semver tags. This only verifies the
explicit-version path, not default discovery; CLI versions through v0.4.51 still
use shared latest and require the channel migration described above:

```sh
METIS_VERSION=v0.4.52 METIS_INSTALL_DIR=/absolute/new/evaluation/bin \
  bash install/install.sh
/absolute/new/evaluation/bin/metis version
```

For that optional route, record the public archive hash, sidecar and unpacked
binary hash as well. Record workflow IDs and results whichever local route is
used. Keep previous attempts unchanged. A 1-hour pilot is not proof of 6-hour
autonomy; do not advertise the latter before it is measured.

No Apple signing credentials are required for this CLI-only release. No npm
registry package is published by these workflows. The updated npm version is
installer source metadata only; `npm publish` remains outside this protocol.

The v0.4.49 build/artifact smokes succeeded, but draft staging failed because
the published-only tag endpoint returned 404 after creating the draft. Keep
that tag/draft and its evidence unchanged; it is not a completed public release.
The v0.4.50 follow-up still failed under Actions tokens because the REST list
omitted its draft despite local OAuth visibility. Keep that tag/draft too;
local OAuth lookup success is not proof of Actions-token behavior.
