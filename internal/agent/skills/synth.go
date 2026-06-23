package skills

// synth.go — controlled write path to ~/.metis/skills/user/. Used
// only by the dreaming subagent (Phase B); a regular user-turn never
// reaches this code because the SkillSynth tool is scoped to the
// dreaming fork's tool registry, not the main agent's.
//
// Design rules (kept narrow so the blast radius of a misbehaved
// dreaming agent is small):
//
//   1. Writes always land in the user-skills dir — bundled and
//      project skills are off-limits.
//   2. Skill names are sanitized: ASCII alnum + dash + underscore
//      only. No `..`, no path separators, no leading dots.
//   3. Frontmatter is generated server-side from a fixed struct —
//      the LLM can't smuggle arbitrary YAML keys (e.g. setting
//      `disabled: false` on a bundled skill it doesn't actually own).
//   4. Create vs Update are explicit — Create refuses to overwrite,
//      Update refuses to create. Prevents a confused agent from
//      silently replacing a hand-tuned user skill.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SynthSkillMeta is the LLM-facing shape of a Skill — narrower than
// pkg/skill.Skill so the dreaming agent can't set fields it has no
// business touching (e.g. UserOnly, Disabled, ModelOverride). What
// makes it past Synth is exactly: name, description, when_to_use,
// dont_use_when, tags, allowed_tools, prompt body.
type SynthSkillMeta struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description,omitempty"`
	WhenToUse    string   `yaml:"when_to_use,omitempty"`
	DontUseWhen  string   `yaml:"dont_use_when,omitempty"`
	Tags         []string `yaml:"tags,omitempty"`
	AllowedTools []string `yaml:"allowed_tools,omitempty"`
	CreatedAt    string   `yaml:"created_at,omitempty"`
}

// Synth is the write-path handle. Construct via NewSynth; thread-safe
// via underlying filesystem semantics (each WriteFile is atomic
// on POSIX so concurrent Create/Update don't tear a single file).
type Synth struct {
	dir    string // typically ~/.metis/skills (the "user" layer dir)
	loader *Loader
	usage  *UsageStore
}

// NewSynth returns a Synth bound to dir (the user-skills directory).
// loader is optional — when non-nil, Create/Update calls Invalidate
// on it so the next List/Get sees the new file without a full
// process restart.
func NewSynth(dir string, loader *Loader) *Synth {
	return &Synth{dir: dir, loader: loader, usage: NewUsageStore(dir)}
}

// Dir is the absolute target directory. Exposed for tests + logs.
func (s *Synth) Dir() string { return s.dir }

// ArchiveSkill retires a redundant user skill into the recoverable
// archive — the consolidation counterpart to CreateSkill. Used by the
// dreaming fork after it merges duplicates into an umbrella skill.
// Routes through the curator so the same validation + .archive/ layout
// + restore path apply.
func (s *Synth) ArchiveSkill(name string) error {
	// No sanitizeName here: that regex (lowercase-only, min length 2) is the
	// rule for *creating* a name, but an existing on-disk skill being
	// retired may be `Go.md` or a single char. Curator.Archive applies the
	// path-safety check (validateSkillName) + case-insensitive file lookup,
	// which is the right gate for an existing file.
	if err := NewCurator(s.dir).Archive(name); err != nil {
		return err
	}
	if s.loader != nil {
		s.loader.Invalidate()
	}
	return nil
}

// nameRE allows ASCII alnum + dash + underscore. No path separators,
// no leading dots (would hide the file), no spaces. The same shape
// Claude Code's autoDream uses for memory file names — keeps the user
// experience symmetric (`ls ~/.metis/skills/` reads cleanly).
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,63}$`)

// sanitizeName lowercases and validates the requested name. Returns
// the cleaned name or an error if the input can't be made safe.
// Refusing rather than fixing is deliberate — silently mangling the
// agent's chosen name produces files the agent can't find again on
// the next dream cycle ("did I save it as foo-bar or foo_bar?").
func sanitizeName(raw string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(raw))
	if n == "" {
		return "", fmt.Errorf("skill name empty")
	}
	if !nameRE.MatchString(n) {
		return "", fmt.Errorf("skill name %q invalid (must match %s)", raw, nameRE.String())
	}
	return n, nil
}

// CreateSkill writes a brand-new skill to ~/.metis/skills/<name>.md.
// Refuses to overwrite — caller must use UpdateSkill to modify an
// existing file. Returns the absolute path on success.
//
// The created file always carries a `created_at: <ISO8601>` stamp so
// future dream cycles can tell "I wrote this last week" from "this
// existed before metis grew the dream loop". meta.CreatedAt is
// ignored if set — the timestamp is server-truth.
func (s *Synth) CreateSkill(meta SynthSkillMeta, body string) (string, error) {
	name, err := sanitizeName(meta.Name)
	if err != nil {
		return "", err
	}
	if s.dir == "" {
		return "", fmt.Errorf("synth: no target dir")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", fmt.Errorf("synth: mkdir %s: %w", s.dir, err)
	}
	path := filepath.Join(s.dir, name+".md")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("synth: skill %q already exists; call update_skill to modify", name)
	}

	meta.Name = name
	meta.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	doc, err := renderFrontmatterDoc(meta, body)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		return "", fmt.Errorf("synth: write %s: %w", path, err)
	}
	// Stamp authoritative provenance + creation time in the usage store so
	// the curator knows this is agent-created (not a hand-written flat .md)
	// and can anchor its lifecycle clock on a real activity timestamp.
	s.usage.RecordCreate(name, SourceAgentSynth)
	if s.loader != nil {
		s.loader.Invalidate()
	}
	return path, nil
}

// UpdateSkill rewrites the body of an existing skill. The frontmatter
// is preserved (the dreaming agent can't accidentally clobber the
// user's hand-tuned description by feeding a smaller meta block).
// Returns the absolute path on success.
//
// To change frontmatter on an existing skill, the agent must
// physically edit the .md via the regular Write/Edit tools — Synth
// stays scoped to "create + body-update" so a misbehaved dream
// can't, e.g., set `disabled: true` on a critical skill.
func (s *Synth) UpdateSkill(name, body string) (string, error) {
	clean, err := sanitizeName(name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.dir, clean+".md")
	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("synth: skill %q does not exist; call create_skill first", clean)
		}
		return "", fmt.Errorf("synth: read %s: %w", path, err)
	}
	// Splice in the new body, keeping the existing frontmatter intact.
	// splitFrontmatter returns the YAML content sans fences and with no
	// trailing newline (it ends right before the closing `\n---\n`), so
	// we have to add both newlines + the closing fence ourselves.
	header, _, hasFM := splitFrontmatter(existing)
	var out []byte
	if hasFM {
		yaml := strings.TrimRight(string(header), "\n")
		out = []byte(fmt.Sprintf("---\n%s\n---\n%s\n", yaml, strings.TrimSpace(body)))
	} else {
		// No frontmatter on disk — write body only (parseMarkdown
		// handles a body-only file fine).
		out = []byte(strings.TrimSpace(body) + "\n")
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "", fmt.Errorf("synth: write %s: %w", path, err)
	}
	// Record the patch event (count + last_patched_at) so the curator sees
	// "recently improved" as activity that keeps the skill fresh.
	s.usage.RecordPatch(clean)
	if s.loader != nil {
		s.loader.Invalidate()
	}
	return path, nil
}

// ListUserSkills returns the names (sans .md extension) of every
// user-layer skill currently on disk. Used by the dream prompt's
// Orient phase so the agent sees "here's what already exists" before
// proposing new skills.
//
// Failures (missing dir, permission errors) collapse to an empty
// slice — Orient phase can't recover from a broken filesystem
// anyway, and dropping the listing is gentler than aborting the
// whole dream over a stat() error.
func (s *Synth) ListUserSkills() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".md") {
			continue
		}
		names = append(names, strings.TrimSuffix(n, ".md"))
	}
	return names
}

// renderFrontmatterDoc serializes meta as YAML frontmatter and appends
// the markdown body, producing a file the existing parseMarkdown
// loader can read back without modification.
func renderFrontmatterDoc(meta SynthSkillMeta, body string) (string, error) {
	yml, err := yaml.Marshal(&meta)
	if err != nil {
		return "", fmt.Errorf("synth: yaml marshal: %w", err)
	}
	return fmt.Sprintf("---\n%s---\n%s\n", string(yml), strings.TrimSpace(body)), nil
}
