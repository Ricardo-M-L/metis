package skills

import "strings"

// isJunkFilename reports whether a filename should be skipped during
// any filesystem (or embed.FS) scan. The single source of truth for the
// "ignore macOS resource forks + editor backups" rule, used by:
//
//   - bundledLayer (embedded.FS scan)
//   - dirLayer (user / project skill dir scan)
//   - Store.List (installed-skill JSON scan)
//
// macOS tar archives leak `._foo.md` (AppleDouble resource forks) into
// the source tree on extraction; the `*.md` embed glob then bundles
// them. Editor backups (`~foo.md`) and `.DS_Store` would similarly
// pollute a directory listing if not filtered.
//
// This is filename-only — the caller has already decided the directory
// is in scope.
func isJunkFilename(name string) bool {
	switch {
	case strings.HasPrefix(name, "._"): // macOS resource forks
		return true
	case strings.HasPrefix(name, "~"): // editor backups (vim, emacs)
		return true
	case name == ".DS_Store":
		return true
	}
	return false
}
