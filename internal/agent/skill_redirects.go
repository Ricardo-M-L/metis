package agent

// skill_redirects.go — when the model Reads a SKILL.md (or any file
// shaped like one), append a <system-reminder> on the tool_result
// reminding it that this is a *procedure to follow when relevant*,
// not a reference doc to browse.
//
// Why this exists: the agent often opens skill manifests "to see
// what's in there" without realising the file IS the trigger — once
// loaded, the body is meant to be executed as guidance for the
// current task, not summarised back to the user. The reminder also
// discourages reading every other skill file in the directory: the
// /skills system already indexes them; mass-Read is wasted tokens.
//
// The detector errs conservative — only fires when the file looks
// unambiguously like a skill (YAML frontmatter with a `name:` field
// AND the path is under a `/skills/` directory). Generic markdown
// files with frontmatter (Jekyll posts, MkDocs pages) don't trip it.

import (
	"path/filepath"
	"strings"
)

// skillReadHint returns a system-reminder body when the (path, output)
// of a Read call looks like a skill manifest, or "" when no nudge
// applies. The output is the raw line-numbered Read result; we strip
// the 6-digit prefix on a per-line basis to peek at frontmatter.
func skillReadHint(path, output string) string {
	if path == "" || output == "" {
		return ""
	}

	// Path gate: must live under a `skills` directory and be markdown.
	// Catches:
	//   ~/.metis/skills/foo.md
	//   ~/.metis/skills/foo/SKILL.md
	//   <repo>/internal/agent/skills/builtin/bar.md
	//   ./.metis/skills/baz/SKILL.md
	if !pathIsSkillManifest(path) {
		return ""
	}

	// Body gate: peek at the first ~12 line-prefixed lines for YAML
	// frontmatter with a `name:` key. Read output has the form
	// "     1\t---\n     2\tname: foo\n..."; we strip the leading
	// number+tab.
	if !bodyLooksLikeSkill(output) {
		return ""
	}

	return "This file is a skill manifest. The body below is a procedure to follow when the task matches its `when_to_use` description — not a reference doc to summarise back to the user. If the current task fits this skill, follow its steps; otherwise carry on. Don't scan other skill files in the directory — the `/skills` index already knows them."
}

// pathIsSkillManifest checks whether a path is plausibly a skill
// manifest based on directory and extension. Conservative — false
// negatives just skip the nudge; false positives waste a few tokens.
func pathIsSkillManifest(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".md" && ext != ".markdown" {
		return false
	}
	// Look for a `/skills/` segment (or `/skills` at end) in the path.
	// Use forward slashes since filepath.ToSlash normalises Windows.
	p := filepath.ToSlash(path)
	if !strings.Contains(p, "/skills/") {
		return false
	}
	return true
}

// bodyLooksLikeSkill returns true when the Read output (with its
// 6-digit-line-prefix format) has YAML frontmatter that contains a
// `name:` key in the first dozen lines. The frontmatter fence is
// `---` on its own line.
func bodyLooksLikeSkill(output string) bool {
	lines := strings.SplitN(output, "\n", 16)
	if len(lines) < 3 {
		return false
	}
	// Strip the "%6d\t" prefix from each line. Read's format is
	// fmt.Fprintf("%6d\t%s\n", lineno, text).
	strip := func(s string) string {
		if i := strings.IndexByte(s, '\t'); i >= 0 {
			return s[i+1:]
		}
		return s
	}
	if strings.TrimSpace(strip(lines[0])) != "---" {
		return false
	}
	// Find the closing fence and check for `name:` in between.
	for i := 1; i < len(lines); i++ {
		body := strings.TrimRight(strip(lines[i]), "\r")
		if strings.TrimSpace(body) == "---" {
			return false // hit closing fence without seeing name:
		}
		if strings.HasPrefix(strings.TrimSpace(body), "name:") {
			return true
		}
	}
	return false
}
