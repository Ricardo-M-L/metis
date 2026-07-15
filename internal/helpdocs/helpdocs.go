// Package helpdocs serves embedded reference documents that the
// `metis <topic>` CLI surfaces (env vars, theme list, etc). One
// markdown file per topic, embedded at build time so the binary
// stays self-contained — users don't need a separate doc install.
//
// Add a new topic by:
//  1. Drop the file under internal/helpdocs/<name>.md
//  2. Wire it in cmd/metis (new case in the dispatch table).
//
// Keep docs SHORT and curated — this isn't a substitute for a real
// docs site. It's the answer the user wants when they're already at
// a terminal and don't want to context-switch to a browser.
package helpdocs

import _ "embed"

//go:embed env.md
var envMD string

// Env returns the curated environment-variable reference shown by
// `metis env`. Source is internal/helpdocs/env.md, embedded at
// build time. Update the markdown when adding / removing /
// renaming a METIS_* variable.
func Env() string { return envMD }
