package skills

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads a skill manifest from disk and dispatches by file extension:
//
//   - .md / .markdown → YAML frontmatter + Markdown body (preferred,
//     git-friendly, hand-editable)
//   - .json           → legacy single-document format (kept for
//     backward compat + GitHubSource wire format)
//
// Returns a *Skill ready for the in-memory store. Filename (sans
// extension) wins as the skill name when frontmatter omits it — this
// mirrors hermes-agent's filesystem-name convention.
func Load(path string) (*Skill, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("skills: read %s: %w", path, err)
	}
	defaultName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	var sk *Skill
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		sk, err = parseMarkdown(b, defaultName)
	case ".json":
		sk, err = parseJSON(b)
	default:
		return nil, fmt.Errorf("skills: %s: unsupported extension (use .md or .json)", path)
	}
	if err != nil {
		return nil, err
	}
	// Stamp the on-disk location so prompt-body template vars
	// (`${METIS_SKILL_DIR}`) can resolve at invoke time.
	if abs, absErr := filepath.Abs(path); absErr == nil {
		sk.LocalPath = abs
	} else {
		sk.LocalPath = path
	}
	return sk, nil
}

// parseMarkdown splits a SKILL.md file into (YAML frontmatter, markdown
// body). The body becomes Skill.Prompt; the frontmatter populates the
// rest of the struct. defaultName is used when frontmatter omits `name`.
//
// Frontmatter is delimited by a leading `---\n` and a closing `---\n`.
// A file without frontmatter is still valid: name = defaultName, prompt =
// entire body. This makes "drop a .md anywhere → it works" the cheap
// path for casual skill authors.
func parseMarkdown(b []byte, defaultName string) (*Skill, error) {
	header, body, hasFrontmatter := splitFrontmatter(b)
	sk := &Skill{Name: defaultName}
	if hasFrontmatter {
		if err := yaml.Unmarshal(header, sk); err != nil {
			return nil, fmt.Errorf("skills: yaml frontmatter: %w", err)
		}
	}
	if sk.Name == "" {
		sk.Name = defaultName
	}
	sk.Prompt = strings.TrimSpace(string(body))
	if sk.Description == "" && sk.Prompt != "" {
		// Lightweight heuristic: first non-empty line of body becomes the
		// description if frontmatter didn't set one. Helps tests and
		// `metis skills list` show *something* for hand-typed skills.
		for _, line := range strings.SplitN(sk.Prompt, "\n", 8) {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				sk.Description = line
				break
			}
		}
	}
	return sk, nil
}

// parseJSON is the legacy single-document loader. Kept because
// internal/agent/skills/marketplace.go's GitHubSource still receives
// JSON manifests over the wire (the .md format is on-disk only).
func parseJSON(b []byte) (*Skill, error) {
	var sk Skill
	if err := json.Unmarshal(b, &sk); err != nil {
		return nil, fmt.Errorf("skills: json decode: %w", err)
	}
	return &sk, nil
}

// splitFrontmatter splits a markdown file into (header bytes, body bytes,
// hasFrontmatter). Frontmatter must start at byte 0 with `---\n`; the
// closing fence is the next standalone `---\n` line.
//
// Tolerates CRLF line endings (Windows authoring). Returns (nil, full
// content, false) when no frontmatter fence is found — the caller then
// treats the whole file as the body.
func splitFrontmatter(b []byte) ([]byte, []byte, bool) {
	// Normalize CRLF to LF so the rest of this function only handles \n.
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(b, []byte("---\n")) {
		return nil, b, false
	}
	rest := b[len("---\n"):]
	// Search for the closing fence as a standalone line.
	closeFence := []byte("\n---\n")
	idx := bytes.Index(rest, closeFence)
	if idx < 0 {
		// Tolerate EOF-terminated frontmatter (no trailing newline).
		if i := bytes.Index(rest, []byte("\n---")); i >= 0 && i+4 == len(rest) {
			return rest[:i], nil, true
		}
		return nil, b, false
	}
	header := rest[:idx]
	body := rest[idx+len(closeFence):]
	return header, body, true
}
