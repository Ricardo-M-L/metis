//go:build !production

package main

import "embed"

// Development, binding generation, and Go tests do not require a pre-built
// Vite bundle. Embedding the source tree keeps those builds self-contained;
// Wails dev serves the frontend through its development server.
//
//go:embed frontend/index.html frontend/src
var assets embed.FS
