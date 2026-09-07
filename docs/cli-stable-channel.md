# CLI stable discovery

The CLI stable channel is independent of GitHub's shared `/releases/latest`
pointer. The shared pointer remains the full CLI + Desktop channel. A published
CLI-only release may have `prerelease=false` and `make_latest=false`: it must
still be discoverable by the CLI.

This fixes the case where a running CLI reports `current: v0.4.51` but its
latest-version hint and default installer still resolve the shared v0.4.47.
Do not hide that mismatch by overwriting the user's version cache.

## Discovery contract: cli-stable-v1

Go self-update (including its TUI hint), the Bash installer, and the PowerShell
installer use the same selection rules:

1. Read `GET /repos/{owner}/{repo}/releases?per_page=100&page=N` from the configured
   GitHub API base, with optional existing authentication. Start at page 1 and
   continue until a page contains fewer than 100 releases.
2. Examine all pages before selecting the numerically highest eligible version;
   list order is not semantic-version order. Do not read shared latest, scrape
   release HTML, or treat bare Git tags as published CLI releases.
3. An eligible tag is exactly `vMAJOR.MINOR.PATCH`. Each component is `0` or a
   nonzero digit followed by up to eight digits. Leading zeros, prerelease
   suffixes, build metadata, and non-version tags are not stable-channel tags.
4. Both `draft` and `prerelease` must be explicitly Boolean false.
5. All twelve CLI assets must be present: Darwin/Linux/Windows × amd64/arm64,
   each archive and its `.sha256` sidecar. Windows uses `.zip`; other platforms
   use `.tar.gz`. Every expected name must occur once, have positive size, be
   in the `uploaded` state, and have the exact configured repository/tag download
   URL. Additional Desktop assets are allowed. An incomplete release is not a
   channel candidate.
   Size is a finite positive integer-valued JSON number (for example, `101.0`
   and `1.01e2` both represent 101 bytes); booleans, strings, and fractions
   are not valid sizes. Invalid candidates do not end pagination prematurely.
6. Bound each response to 8 MiB and the scan to ten pages. A full tenth page,
   failed subsequent page, malformed response, or deadline is a discovery
   failure, not permission to return a partial answer. Go uses its one overall
   10-second metadata budget; installers use one overall 30-second budget.

The GitHub endpoint and pagination parameters are described in the
[official Releases API documentation](https://docs.github.com/en/rest/releases/releases#list-releases).
Shared conformance examples live in
`install/testdata/cli-releases.json`; each implementation must consume them.

## Failure and installation safety

There is no fallback to shared latest when CLI discovery fails. In particular,
anonymous API rate limits must not silently turn CLI latest back into v0.4.47.
Background checks retain existing state without recording a successful check;
explicit checks/default installs report the failure. An existing GitHub token
can raise API limits. Explicitly pinning a known release remains available.
The cache records its protocol and a fingerprint of the effective repository,
API base, and download base. Changing any source invalidates the old successful
check and notification state instead of reusing it for another repository.

The Bash default resolver needs `jq` or `python3` for structured JSON parsing.
It never executes a downloaded parser and never approximates nested release
metadata with grep/sed. If neither parser is available, the diagnostic explains
the dependency; a pinned `METIS_VERSION` installation still works without it.
PowerShell uses its built-in JSON parser.

Discovery is not verification of downloaded bytes. Existing checksum, archive
limits, reported-binary-version checks, immutable version slots, process locks,
and atomic activation remain mandatory. Automatic updates cannot move an
already activated newer CLI backward. `metis update --force` means reinstall
the same stable version, not downgrade to an older available release.
Default `latest` installers also compare the verified current installation
under their installation lock, before downloading an archive, and reject
implicit downgrades. Explicitly pinned installer versions remain deliberate
version selections and retain their existing behavior.

## Migrating installed v0.4.51 and earlier

An old binary cannot learn a new discovery protocol from a source-code change.
Without changing Desktop's shared latest pointer, old installations need one
upgrade through the updated installer (or an explicitly pinned, verified local
build of the new published tag). Subsequent launches use the new CLI channel.
Do not recommend an old binary's `update --force` as a migration mechanism.

For this user's machine, retain the selected no-download workflow: build the
new immutable published tag locally, record its full commit and binary hash,
then activate it through the existing versioned CLI installation mechanism.
Do not claim a development build or an unpublished patch is a released CLI.

## Evaluation isolation

The registered v0.4.51 pilot has its own frozen binary, source, and evaluator
hashes. Do not replace those files, change its clocks, or attribute this updater
patch to that run. A later independently registered run must state the exact
CLI version it actually used. Old failures and releases remain unchanged.
