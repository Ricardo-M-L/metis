package version

// Version / Commit / Date are set at build time via -ldflags -X (see Makefile).
// The defaults below are what `go install ./cmd/metis` produces when the
// build doesn't go through `make`.
var (
	Version = "0.1.0"
	Commit  = ""
	Date    = ""
)
