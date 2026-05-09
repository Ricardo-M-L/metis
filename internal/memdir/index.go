package memdir

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
)

// IndexEntry is a single bullet in MEMORY.md. Format:
//
//	- [Title](file.md) — short hook
//
// Title and Hook are optional descriptive parts. File is the relative
// filename (within memdir root). Lines that don't conform to the
// pattern are returned as Raw and skipped during regenerate-from-files.
type IndexEntry struct {
	Title string
	File  string
	Hook  string
	Raw   string // populated when the line failed to parse cleanly
}

// ReadIndex parses MEMORY.md if present. Returns ([], nil) when the
// index doesn't exist — that's a valid state (the index is regenerated
// on every memdir write, but a fresh memdir starts without one).
//
// The first 200 lines are honored as a soft cap mirroring the upstream
// guidance ("MEMORY.md is always loaded into context — lines after 200
// will be truncated"). Beyond that we still parse but flag oversize
// via the returned bool — callers can warn or trigger a rewrite.
func ReadIndex(root string) (entries []IndexEntry, oversize bool, err error) {
	b, err := os.ReadFile(IndexPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(b))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineCount := 0
	for scanner.Scan() {
		lineCount++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Bullet: `-` or `*` lead, allow leading whitespace.
		if !(strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ")) {
			entries = append(entries, IndexEntry{Raw: line})
			continue
		}
		body := strings.TrimSpace(line[2:])
		entry := parseBulletBody(body)
		if entry.File == "" {
			entry.Raw = body
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	return entries, lineCount > 200, nil
}

// parseBulletBody pulls (Title, File, Hook) out of one of:
//
//	[Title](file.md) — Hook
//	[Title](file.md) - Hook
//	[Title](file.md)
//	file.md — Hook
//	file.md
//
// Tolerates either em-dash or hyphen as the hook separator and either
// `—` (U+2014) or ` - `.
func parseBulletBody(body string) IndexEntry {
	var e IndexEntry

	// Markdown link form first: [title](file)
	if strings.HasPrefix(body, "[") {
		closeBracket := strings.Index(body, "]")
		if closeBracket > 0 && closeBracket+1 < len(body) && body[closeBracket+1] == '(' {
			closeParen := strings.Index(body[closeBracket+1:], ")")
			if closeParen > 0 {
				e.Title = body[1:closeBracket]
				e.File = body[closeBracket+2 : closeBracket+1+closeParen]
				rest := strings.TrimSpace(body[closeBracket+2+closeParen:])
				e.Hook = trimHookSeparator(rest)
				return e
			}
		}
	}

	// Bare-filename form: "file.md — hook"
	if strings.Contains(body, " — ") || strings.Contains(body, " - ") {
		// Split on first separator.
		sep := " — "
		idx := strings.Index(body, sep)
		if idx < 0 {
			sep = " - "
			idx = strings.Index(body, sep)
		}
		if idx >= 0 {
			file := strings.TrimSpace(body[:idx])
			hook := strings.TrimSpace(body[idx+len(sep):])
			if strings.HasSuffix(file, ".md") {
				e.File = file
				e.Hook = hook
				return e
			}
		}
	}
	if strings.HasSuffix(body, ".md") {
		e.File = body
	}
	return e
}

func trimHookSeparator(s string) string {
	s = strings.TrimSpace(s)
	for _, sep := range []string{" — ", "— ", " - ", "- "} {
		if strings.HasPrefix(s, sep) {
			return strings.TrimSpace(s[len(sep):])
		}
	}
	return s
}

// WriteIndex regenerates MEMORY.md from a list of files. Atomic:
// writes to a sibling tmp file then renames, so a crashed extractor
// can't leave a half-written index that breaks the next ReadIndex.
//
// The output is sorted by type (user, feedback, project, reference,
// unclassified) then by filename within each type. Each line is
// `- [<frontmatter.name>](<basename>) — <description>`. Files with
// parse errors are dropped from the index — they remain on disk and
// show up in the manifest's "errors" section but don't pollute the
// curated index.
func WriteIndex(root string, files []MemoryFile) error {
	var sb strings.Builder

	order := []MemoryType{TypeUser, TypeFeedback, TypeProject, TypeReference, ""}
	for _, t := range order {
		var section []MemoryFile
		for _, f := range files {
			if f.ParseError != nil {
				continue
			}
			if f.Frontmatter.Type == t {
				section = append(section, f)
			}
		}
		if len(section) == 0 {
			continue
		}
		sort.SliceStable(section, func(i, j int) bool {
			return section[i].Name < section[j].Name
		})
		for _, f := range section {
			title := f.Frontmatter.Name
			if title == "" {
				title = f.Name
			}
			file := baseName(f.Path)
			desc := strings.TrimSpace(f.Frontmatter.Description)
			if desc == "" {
				fmt.Fprintf(&sb, "- [%s](%s)\n", title, file)
			} else {
				fmt.Fprintf(&sb, "- [%s](%s) — %s\n", title, file, desc)
			}
		}
	}

	tmp := IndexPath(root) + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, IndexPath(root))
}
