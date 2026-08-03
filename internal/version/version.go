package version

import "strings"

// Version / Commit / Date are set at build time via -ldflags -X (see Makefile).
// The defaults below are what `go install ./cmd/metis` produces when the
// build doesn't go through `make`.
var (
	Version = "0.4.0"
	Commit  = ""
	Date    = ""
)

// Short returns the user-facing form of `Version` — semver only, no git
// describe noise. `v0.1.3-21-gab7a825-dirty` → `0.1.3`. The full string
// stays available via the `Version` global for `-V` / build fingerprint
// paths. Pulled into one helper after the comparison run found two
// places (help-tab header, `metis version`) shipping the noisy form.
func Short() string { return shortenSemver(Version) }

// shortenSemver is the pure transform used by Short(); split out so
// unit tests can pin the strip behavior without mutating the package
// global. Drops a leading `v` and any `-…` suffix (git-describe rev,
// pre-release tags, dirty marker) since the linker passes the tag
// form straight through.
func shortenSemver(s string) string {
	v := strings.TrimPrefix(s, "v")
	if i := strings.Index(v, "-"); i >= 0 {
		return v[:i]
	}
	return v
}
