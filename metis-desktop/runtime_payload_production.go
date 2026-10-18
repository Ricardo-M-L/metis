//go:build production

package main

import (
	"embed"
	"io/fs"
)

// Generated target payloads accompany the signed Desktop build. README alone
// keeps build tooling usable; a missing target is rejected at runtime.
//
//go:embed all:embedded-runtime
var desktopRuntimeEmbedded embed.FS
var desktopRuntimePayloadFS fs.FS = desktopRuntimeEmbedded

const desktopRuntimeProduction = true
