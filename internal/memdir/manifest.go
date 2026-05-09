package memdir

import (
	"fmt"
	"sort"
	"strings"
)

// FormatManifest renders a list of memory files as a compact text
// summary suitable for injecting into the forked extractor agent's
// first user message. Mirrors openclaude's formatMemoryManifest.
//
// Format:
//
//   <root>: 5 memories (3 user, 1 feedback, 1 reference)
//
//   ## user (3)
//   - user_role.md — Backend lead at goalfy, focused on metis CLI
//   - user_timezone.md — Lives in UTC+8, prefers Chinese for chat
//   - user_editor.md — Uses neovim
//
//   ## feedback (1)
//   - feedback_chinese_only.md — All chat replies in Chinese; code stays English
//
//   ## reference (1)
//   - reference_oa_databases.md — OA Postgres connection info per env
//
// Files with parse errors land in a "## errors" section at the bottom
// so the extractor agent can fix them. Files without a Type land in
// "## unclassified".
func FormatManifest(root string, files []MemoryFile) string {
	if len(files) == 0 {
		return fmt.Sprintf("%s: (empty)\n", root)
	}

	var (
		buckets   = map[MemoryType][]MemoryFile{}
		unclass   []MemoryFile
		withError []MemoryFile
	)
	for _, f := range files {
		if f.ParseError != nil {
			withError = append(withError, f)
			continue
		}
		t := f.Frontmatter.Type
		switch t {
		case TypeUser, TypeFeedback, TypeProject, TypeReference:
			buckets[t] = append(buckets[t], f)
		default:
			unclass = append(unclass, f)
		}
	}

	var sb strings.Builder
	totalKnown := len(buckets[TypeUser]) + len(buckets[TypeFeedback]) +
		len(buckets[TypeProject]) + len(buckets[TypeReference])
	fmt.Fprintf(&sb, "%s: %d memories", root, len(files))
	parts := []string{}
	if n := len(buckets[TypeUser]); n > 0 {
		parts = append(parts, fmt.Sprintf("%d user", n))
	}
	if n := len(buckets[TypeFeedback]); n > 0 {
		parts = append(parts, fmt.Sprintf("%d feedback", n))
	}
	if n := len(buckets[TypeProject]); n > 0 {
		parts = append(parts, fmt.Sprintf("%d project", n))
	}
	if n := len(buckets[TypeReference]); n > 0 {
		parts = append(parts, fmt.Sprintf("%d reference", n))
	}
	if n := len(unclass); n > 0 {
		parts = append(parts, fmt.Sprintf("%d unclassified", n))
	}
	if n := len(withError); n > 0 {
		parts = append(parts, fmt.Sprintf("%d errored", n))
	}
	if totalKnown != len(files) || len(parts) > 0 {
		fmt.Fprintf(&sb, " (%s)", strings.Join(parts, ", "))
	}
	sb.WriteString("\n\n")

	order := []MemoryType{TypeUser, TypeFeedback, TypeProject, TypeReference}
	for _, t := range order {
		section := buckets[t]
		if len(section) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "## %s (%d)\n", t, len(section))
		writeBullets(&sb, section)
		sb.WriteString("\n")
	}

	if len(unclass) > 0 {
		fmt.Fprintf(&sb, "## unclassified (%d)\n", len(unclass))
		writeBullets(&sb, unclass)
		sb.WriteString("\n")
	}
	if len(withError) > 0 {
		fmt.Fprintf(&sb, "## errors (%d)\n", len(withError))
		for _, f := range withError {
			fmt.Fprintf(&sb, "- %s — parse: %v\n", baseName(f.Path), f.ParseError)
		}
	}
	return sb.String()
}

func writeBullets(sb *strings.Builder, files []MemoryFile) {
	// Stable sort within section: by mtime desc (already sorted by
	// ScanMemoryFiles, but FilterByType callers may pass a different
	// slice — re-sort to be safe and not depend on caller ordering).
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].ModTime.After(files[j].ModTime)
	})
	for _, f := range files {
		desc := strings.TrimSpace(f.Frontmatter.Description)
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(sb, "- %s — %s\n", baseName(f.Path), desc)
	}
}

func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
