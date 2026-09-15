package main

import (
	"os"
	"path/filepath"
	"strings"
)

// The host passes only its own resource directory. The backend treats this as
// a location hint and verifies archive/executable hashes against its catalog.
func computerUseBundleEnvironment(environment []string, executable string) []string {
	out := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "METIS_CU_BUNDLE_DIR=") {
			out = append(out, entry)
		}
	}
	if executable == "" || !filepath.IsAbs(executable) {
		return out
	}
	macos := filepath.Dir(executable)
	contents := filepath.Dir(macos)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" {
		return out
	}
	bundle := filepath.Join(contents, "Resources", "computer-use")
	if info, err := os.Stat(bundle); err == nil && info.IsDir() {
		out = append(out, "METIS_CU_BUNDLE_DIR="+bundle)
	}
	return out
}
