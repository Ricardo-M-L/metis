//go:build production

package main

import "embed"

// Production Wails builds run the configured frontend build first, then embed
// the generated bundle. Keeping this behind the production tag means a clean
// checkout can run Go tests before npm has created frontend/dist.
//
//go:embed all:frontend/dist
var assets embed.FS
