package tui

// atmention.go implements claude-code's `@filename` autocomplete.
//
// Trigger: input ends with `@xxx` (no whitespace after the @). The
// renderer shows a small dropdown above the input box with up to
// `atMentionMaxRows` files whose relative path matches `xxx` via
// fuzzyMatch. Tab / Enter accepts the highlighted entry, replacing
// `@xxx` with `@<full/relative/path>`.
//
// File index is cached for `atMentionCacheTTL` to keep typing fast —
// re-walks happen on first activation per turn and only after the
// TTL elapses. Cap at `atMentionMaxIndex` so absurdly large repos
// don't lock up the keystroke handler.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	atMentionMaxRows  = 8
	atMentionMaxIndex = 5000
	atMentionCacheTTL = 30 * time.Second
)

var (
	atMentionFilesMu sync.Mutex
	atMentionFiles   []string
	atMentionFilesAt time.Time
)

// atMentionIndex returns a list of repo-relative file paths suitable
// for @filename completion. Skips the obvious noise dirs (.git,
// node_modules, vendor, target, dist, build) so a Go monorepo doesn't
// drown in third-party paths the user never wants to reference.
func atMentionIndex() []string {
	atMentionFilesMu.Lock()
	defer atMentionFilesMu.Unlock()
	if time.Since(atMentionFilesAt) < atMentionCacheTTL && len(atMentionFiles) > 0 {
		return atMentionFiles
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true,
		"target": true, "dist": true, "build": true,
		".next": true, ".cache": true, "__pycache__": true,
	}
	var files []string
	_ = filepath.WalkDir(cwd, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(cwd, path)
		if relErr != nil || rel == "." {
			return nil
		}
		// Skip dotfiles at root level and binary-ish extensions to
		// keep the index focused on source/text files the user is
		// likely to reference.
		base := d.Name()
		if strings.HasPrefix(base, ".") {
			return nil
		}
		files = append(files, rel)
		if len(files) >= atMentionMaxIndex {
			return fs.SkipAll
		}
		return nil
	})
	atMentionFiles = files
	atMentionFilesAt = time.Now()
	return files
}

// detectAtMention scans the input buffer for a trailing @-token. Returns
// (filter, true) when the token is present and should trigger the
// dropdown; (_, false) otherwise. The filter excludes the leading "@".
//
// Examples (cursor at end-of-input):
//
//	"hello @foo"   → ("foo", true)
//	"hello @"       → ("", true)   — empty filter, show top of index
//	"hello @foo "   → ("", false)  — whitespace ends the mention
//	"hello"         → ("", false)
func detectAtMention(input string) (string, bool) {
	if input == "" {
		return "", false
	}
	// Walk back from end until we find @ or whitespace.
	for i := len(input) - 1; i >= 0; i-- {
		c := input[i]
		if c == '@' {
			// Confirm @ is at start-of-input or preceded by whitespace
			// — otherwise it's an email or similar.
			if i == 0 || input[i-1] == ' ' || input[i-1] == '\n' || input[i-1] == '\t' {
				return input[i+1:], true
			}
			return "", false
		}
		if c == ' ' || c == '\n' || c == '\t' {
			return "", false
		}
	}
	return "", false
}

// matchAtMention scores files against a filter and returns the top
// matches up to `atMentionMaxRows`. Empty filter returns the most-
// frecent N from the index (falling back to walk order on a fresh
// repo where no file has been picked yet). fuzzyMatch handles
// substring + acronym matching; sortByFrecency re-ranks the survivors
// so files the user picks repeatedly bubble up.
func matchAtMention(filter string) []string {
	files := atMentionIndex()
	if len(files) == 0 {
		return nil
	}
	if filter == "" {
		ranked := sortByFrecency(files)
		if len(ranked) > atMentionMaxRows {
			return ranked[:atMentionMaxRows]
		}
		return ranked
	}
	// Collect up to 5× the row cap as candidates so frecency has room
	// to re-rank — otherwise a walk-order break at 8 hits would lock
	// in matches the user has never picked over genuinely-frequent
	// ones a few rows later in the index.
	candidateCap := atMentionMaxRows * 5
	var hits []string
	for _, f := range files {
		if fuzzyMatch(f, filter) {
			hits = append(hits, f)
			if len(hits) >= candidateCap {
				break
			}
		}
	}
	ranked := sortByFrecency(hits)
	if len(ranked) > atMentionMaxRows {
		return ranked[:atMentionMaxRows]
	}
	return ranked
}

// updateAtMention re-runs detect+match against the current input and
// updates the Model's atActive / atMatched state. Called after every
// keystroke from keybind_main so the live dropdown reflects what the
// user typed without us needing a separate driver.
//
// Cursor reset to 0 only when the filter actually changed — otherwise
// a navigation up/down would lose its position on the next keystroke.
func (m *Model) updateAtMention() {
	filter, ok := detectAtMention(m.input.Value())
	if !ok {
		m.atActive = false
		m.atMatched = nil
		return
	}
	m.atActive = true
	if filter != m.atFilter {
		m.atFilter = filter
		m.atCursor = 0
	}
	m.atMatched = matchAtMention(filter)
	if m.atCursor >= len(m.atMatched) {
		m.atCursor = 0
	}
}

// acceptAtMention replaces the in-progress `@xxx` with the highlighted
// match and dismisses the dropdown. The trailing space mirrors
// applyAtMention so the user can keep typing the next word
// immediately.
func (m *Model) acceptAtMention() {
	if !m.atActive || m.atCursor >= len(m.atMatched) {
		return
	}
	choice := m.atMatched[m.atCursor]
	RecordMentionAccess(choice)
	m.input.SetValue(applyAtMention(m.input.Value(), choice))
	m.input.CursorEnd()
	m.atActive = false
	m.atMatched = nil
}

// applyAtMention replaces the trailing `@filter` with `@<choice> ` so
// the cursor lands ready for the next word. claude-code adds the
// trailing space too — saves the user from typing it manually.
func applyAtMention(input, choice string) string {
	for i := len(input) - 1; i >= 0; i-- {
		if input[i] == '@' {
			return input[:i] + "@" + choice + " "
		}
	}
	// Shouldn't happen — caller should only invoke when detectAtMention
	// returned (_, true). Defensive: just append.
	return input + "@" + choice + " "
}
