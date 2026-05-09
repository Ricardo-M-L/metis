package memdir

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// MemoryType is the four-way classifier copied from Claude Code's auto-
// memory taxonomy. Each type has different lifetime and use-case:
//
//   - User: durable facts about who the user is (role, expertise,
//     long-running responsibilities). Rarely changes.
//   - Feedback: "do this / don't do that" guidance the user has given.
//     Includes a "why" line so judgment calls in edge cases are possible.
//   - Project: in-flight initiatives, deadlines, decisions. Decay fast;
//     a "Why" line lets future-you decide whether the memory is still
//     load-bearing.
//   - Reference: pointers into external systems (Linear projects, slack
//     channels, dashboards). Stable but external; verify before recommending.
//
// Anything that doesn't fit one of these is a sign it shouldn't be a
// memory at all (it's probably already in the code, git log, or the
// current conversation).
type MemoryType string

const (
	TypeUser      MemoryType = "user"
	TypeFeedback  MemoryType = "feedback"
	TypeProject   MemoryType = "project"
	TypeReference MemoryType = "reference"
)

// IsValid reports whether t is one of the four canonical types.
// Unknown types are still allowed (we don't reject the file), but the
// extractor prompt steers towards these four — the Manifest renderer
// flags unknowns so the model can fix them on the next pass.
func (t MemoryType) IsValid() bool {
	switch t {
	case TypeUser, TypeFeedback, TypeProject, TypeReference:
		return true
	}
	return false
}

// Frontmatter is the parsed YAML header. Fields are openclaude-parity:
// name / description / type are required for a "good" memory;
// originSessionId is informational (which session first wrote this).
//
// Unknown fields are preserved in Extra so the extractor can round-trip
// frontmatter without losing custom keys (e.g. team-memory's
// `team: <name>`).
type Frontmatter struct {
	Name            string         `yaml:"name"`
	Description     string         `yaml:"description"`
	Type            MemoryType     `yaml:"type"`
	OriginSessionID string         `yaml:"originSessionId,omitempty"`
	Extra           map[string]any `yaml:",inline"`
}

// Validate checks the three required fields are present. Returns nil
// for valid memos. Caller decides whether to reject (Scan does NOT —
// it returns the file with a Validate-level "errors" tag so the model
// can fix it next pass).
func (fm *Frontmatter) Validate() error {
	if fm == nil {
		return errors.New("memdir: nil frontmatter")
	}
	if strings.TrimSpace(fm.Name) == "" {
		return errors.New("memdir: missing `name`")
	}
	if strings.TrimSpace(fm.Description) == "" {
		return errors.New("memdir: missing `description`")
	}
	if fm.Type == "" {
		return errors.New("memdir: missing `type`")
	}
	return nil
}

// ParseFile splits raw memory-file bytes into frontmatter + body.
// Returns (fm, body, error) where error is non-nil only on YAML parse
// failure — the absence of frontmatter or invalid required fields are
// surfaced via the returned Frontmatter (caller calls Validate).
//
// A file without frontmatter is still parsed: fm is empty, body is the
// full file. The caller can decide to reject it — extractMemories does
// (an unframed memory is a sign the model fumbled the format).
func ParseFile(b []byte) (*Frontmatter, []byte, error) {
	header, body, has := splitFrontmatter(b)
	fm := &Frontmatter{}
	if !has {
		return fm, body, nil
	}
	if err := yaml.Unmarshal(header, fm); err != nil {
		return fm, body, fmt.Errorf("memdir: yaml: %w", err)
	}
	return fm, body, nil
}

// RenderFile is the inverse of ParseFile: produce a markdown file
// with frontmatter from a Frontmatter struct + body string. Used by
// callers that want to programmatically write memdir files (the
// forked agent uses Write/Edit tool directly, but `metis memory add`
// CLI shortcut goes through this).
func RenderFile(fm *Frontmatter, body string) ([]byte, error) {
	if err := fm.Validate(); err != nil {
		return nil, err
	}
	headerBytes, err := yaml.Marshal(fm)
	if err != nil {
		return nil, fmt.Errorf("memdir: marshal frontmatter: %w", err)
	}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(bytes.TrimRight(headerBytes, "\n"))
	buf.WriteString("\n---\n\n")
	buf.WriteString(strings.TrimSpace(body))
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// splitFrontmatter is a duplicate of internal/agent/skills/manifest.go's
// version. Duplication is preferred over a shared `internal/markdown/`
// package because (a) it's 20 lines, (b) the two callers may diverge
// (e.g. memdir might want to support TOML frontmatter someday), and
// (c) cross-`internal/` import would otherwise pull skills' transitive
// deps into anywhere using memdir.
func splitFrontmatter(b []byte) ([]byte, []byte, bool) {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(b, []byte("---\n")) {
		return nil, b, false
	}
	rest := b[len("---\n"):]
	closeFence := []byte("\n---\n")
	idx := bytes.Index(rest, closeFence)
	if idx < 0 {
		if i := bytes.Index(rest, []byte("\n---")); i >= 0 && i+4 == len(rest) {
			return rest[:i], nil, true
		}
		return nil, b, false
	}
	header := rest[:idx]
	body := rest[idx+len(closeFence):]
	return header, body, true
}
