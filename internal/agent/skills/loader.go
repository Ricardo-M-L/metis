package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	pubskill "github.com/Ricardo-M-L/metis/pkg/skill"
)

// Layer is one priority-tagged source the multi-source loader scans. The
// `Scan` callback returns the skills the layer currently has; `Priority`
// determines who wins when two layers contribute a skill of the same name
// (higher wins). Conventional priority floor → ceiling:
//
//	bundled(0) < optional(5) < user(10) < project(20) < plugin(30) < mcp(40)
//
// optional sits between bundled and user: it's "official but not on by
// default" — same trust level as built-ins but the user opts in via
// `metis skills install <name>`. Mirrors hermes-agent's
// optional-skills/ tree.
//
// Trust level is the loader's posture toward this layer's content,
// stamped onto every skill emitted by Scan. Per-skill TrustLevel set
// in the manifest is overwritten — a community-source skill claiming
// "builtin" trust gets demoted to the layer's actual posture.
type Layer struct {
	Name     string
	Priority int
	Trust    string // pubskill.Trust* constant; falls back to "user" when empty
	Scan     func() ([]Skill, error)
}

// Loader is the multi-source skill aggregator that the agent loop reads
// at prompt-render time. Layers are sorted by Priority once at construct
// time; later .Invalidate() calls re-trigger the scan but keep the order.
//
// Inspired by claude-code's loadSkillsDir.ts (which folds bundled +
// disk + plugin + mcp into one stream) and openclaude's bundledSkills
// in-memory map. We pick the same multi-source story so the user can
// override any built-in skill by dropping a same-named .md into
// ~/.metis/skills/.
type Loader struct {
	Layers []Layer
	Logger func(format string, args ...any)
	// Cwd is the path skills' ActivateOn patterns match against. Set
	// at construct time (typically os.Getwd at session boot). Empty
	// disables ActivateOn filtering — every skill is always-on.
	Cwd string

	mu    sync.RWMutex
	cache []Skill
	dirty bool
}

// NewLoader builds the standard 5-layer loader (bundled / optional /
// user / project / plugin). The MCP layer is omitted when mcp is nil
// — kept as a separate constructor option so unit tests don't need
// an MCP client.
//
// `userDir` is typically `~/.metis/skills`; `projectDir` is
// `$PWD/.metis/skills` (resolved at construct time); `optionalDir`
// is typically `~/.metis/optional-skills` (where `metis skills install
// --optional <name>` lands official-but-not-default skills). Pass
// empty string to disable any layer.
func NewLoader(userDir, projectDir string, plugins []PluginSkillSource) *Loader {
	return NewLoaderWithOptional(userDir, projectDir, "", plugins)
}

// NewLoaderWithOptional is the option-bearing variant — adds an
// "optional" layer between bundled and user. Mirrors hermes-agent's
// optional-skills tree.
func NewLoaderWithOptional(userDir, projectDir, optionalDir string, plugins []PluginSkillSource) *Loader {
	layers := []Layer{
		bundledLayer(),
	}
	if optionalDir != "" {
		layers = append(layers, dirLayer("optional", 5, optionalDir, pubskill.TrustTrusted))
	}
	if userDir != "" {
		layers = append(layers, dirLayer("user", 10, userDir, pubskill.TrustUser))
	}
	if projectDir != "" {
		layers = append(layers, dirLayer("project", 20, projectDir, pubskill.TrustProject))
	}
	if len(plugins) > 0 {
		layers = append(layers, pluginLayer(plugins))
	}
	sort.Slice(layers, func(i, j int) bool { return layers[i].Priority < layers[j].Priority })
	cwd, _ := os.Getwd()
	return &Loader{
		Layers: layers,
		Logger: func(string, ...any) {},
		Cwd:    cwd,
		dirty:  true,
	}
}

// PluginSkillSource is implemented by plugins that contribute skills via
// the multi-source loader. Defined here to avoid a dependency on
// internal/runtime; plugin.Plugin satisfies it via duck typing.
type PluginSkillSource interface {
	Name() string
	Skills() []Skill
}

// Invalidate forces a re-scan on the next List/Get call. Called by
// `metis skills install`, plugin reload, etc.
func (l *Loader) Invalidate() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dirty = true
}

// matchesActivateOn reports whether cwd matches any of the skill's
// ActivateOn patterns. Empty patterns = always-active.
//
// Pattern grammar (intentionally simpler than full doublestar to
// avoid pulling that dep in this package):
//
//   - `**`          matches any sequence of characters (including /)
//   - everything else literal
//
// Examples:
//   - `**/frontend/**` matches `/Users/x/work/myapp/frontend/src/main.tsx`
//   - `**/.go-project` matches a path ending in .go-project
//   - `/Users/x/work/**` matches anything under that prefix
//
// The matcher is greedy-from-left: pattern segments split by `**`
// must appear in order in the path. Adequate for skill-activation
// configuration; complex glob needs (`?`, `[abc]`, single-`*`) are
// out of scope.
func matchesActivateOn(patterns []string, cwd string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if pathPatternMatch(p, cwd) {
			return true
		}
	}
	return false
}

// pathPatternMatch implements the simple **-only glob described
// above. Returns false on empty pattern (defensive — a skill that
// declared activate_on but left it empty per-entry should not match
// everything).
func pathPatternMatch(pattern, path string) bool {
	if pattern == "" {
		return false
	}
	// Special: bare "**" is "match anything".
	if pattern == "**" {
		return true
	}
	parts := strings.Split(pattern, "**")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(path[pos:], part)
		if idx < 0 {
			return false
		}
		// First non-empty part must anchor unless the pattern
		// started with `**`.
		if i == 0 && idx != 0 {
			return false
		}
		pos += idx + len(part)
	}
	// If pattern didn't end in `**`, the last literal must end the path.
	if !strings.HasSuffix(pattern, "**") && pos != len(path) {
		return false
	}
	return true
}

// List returns every skill from every layer, with same-name conflicts
// resolved by priority (later layer wins). Cache is populated once per
// Invalidate cycle.
func (l *Loader) List() ([]Skill, error) {
	l.mu.RLock()
	if !l.dirty && l.cache != nil {
		out := append([]Skill(nil), l.cache...)
		l.mu.RUnlock()
		return out, nil
	}
	l.mu.RUnlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.dirty && l.cache != nil {
		return append([]Skill(nil), l.cache...), nil
	}

	type winner struct {
		sk    Skill
		layer string
	}
	seen := map[string]winner{}
	// Iterate ascending priority — later layer's value overwrites earlier.
	for _, layer := range l.Layers {
		skills, err := layer.Scan()
		if err != nil {
			l.Logger("skill layer %q scan error: %v", layer.Name, err)
			continue
		}
		for _, sk := range skills {
			// Stamp the layer's trust posture onto the skill,
			// overriding any value the manifest tried to set itself
			// (a community skill can't claim builtin trust by writing
			// `trust_level: builtin` in its frontmatter).
			if layer.Trust != "" {
				sk.TrustLevel = layer.Trust
			}
			// Safety scan: mirrors hermes' skills_guard. A skill
			// matching obvious injection / exfil patterns is logged
			// and DROPPED — better silently unavailable than serving
			// the user a poisoned prompt. Bundled / project skills
			// are exempt (ScanSkill skips them).
			if report := ScanSkill(&sk); !report.IsClean() {
				l.Logger("skill %q from layer %q REJECTED by safety scan: %s",
					sk.Name, layer.Name, strings.Join(report.Issues, ", "))
				continue
			}
			// Conditional activation by cwd path (CC-D): skills
			// declaring ActivateOn patterns are filtered out unless
			// the loader's Cwd matches one of them. Empty Cwd
			// disables filtering (always-on, legacy).
			if l.Cwd != "" && len(sk.ActivateOn) > 0 && !matchesActivateOn(sk.ActivateOn, l.Cwd) {
				l.Logger("skill %q: skipped at cwd %q (no ActivateOn match)", sk.Name, l.Cwd)
				continue
			}
			// Disabled skills stay on disk but are pulled out of the
			// system prompt. Matches MCPServerEntry.Disabled — same
			// "iterate without re-installing" affordance.
			if sk.Disabled {
				l.Logger("skill %q: skipped (disabled in manifest)", sk.Name)
				continue
			}
			if prev, ok := seen[sk.Name]; ok && prev.layer != layer.Name {
				l.Logger("skill %q: %s overrides %s", sk.Name, layer.Name, prev.layer)
			}
			seen[sk.Name] = winner{sk: sk, layer: layer.Name}
		}
	}
	out := make([]Skill, 0, len(seen))
	for _, w := range seen {
		out = append(out, w.sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	l.cache = out
	l.dirty = false
	return append([]Skill(nil), out...), nil
}

// Get returns one skill by name, or (nil, nil) when missing. Errors are
// reserved for layer scan failures.
func (l *Loader) Get(name string) (*Skill, error) {
	all, err := l.List()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, nil
}

// RenderForPrompt builds the system-prompt fragment that lists every
// available skill, in XML form (CR-B). Mirrors crush / openclaw /
// opencode — Anthropic / claude-code-trained models pattern-match XML
// far more reliably than markdown bullets when picking which skill
// to invoke.
//
// Output shape:
//
//	<available_skills>
//	  <skill>
//	    <name>debug</name>
//	    <description>...</description>
//	    <when_to_use>...</when_to_use>      <!-- optional -->
//	    <dont_use_when>...</dont_use_when>  <!-- optional -->
//	    <category>...</category>            <!-- optional -->
//	    <trust>builtin|trusted|user|project|community</trust>
//	    <unavailable>missing command:kubectl</unavailable>  <!-- optional -->
//	  </skill>
//	  ...
//	</available_skills>
//
// XML escaping uses encoding/xml so user-supplied text (description,
// when_to_use) can't break the tag structure. UserOnly skills are
// excluded, same as in the prior markdown impl.
func (l *Loader) RenderForPrompt() (string, error) {
	skills, err := l.List()
	if err != nil || len(skills) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString("<available_skills>\n")
	for _, sk := range skills {
		// UserOnly skills are hidden from the LLM-facing prompt — the
		// user can still trigger them via /skill X but the model won't
		// see them in its choice menu.
		if sk.UserOnly {
			continue
		}
		// Prerequisite check: missing → emit <unavailable> tag.
		var missing []string
		if sk.Prerequisites != nil {
			missing = CheckPrerequisites(sk.Prerequisites)
		}
		b.WriteString("  <skill>\n")
		b.WriteString("    <name>")
		b.WriteString(xmlEscape(sk.Name))
		b.WriteString("</name>\n")
		if sk.Description != "" {
			b.WriteString("    <description>")
			b.WriteString(xmlEscape(sk.Description))
			b.WriteString("</description>\n")
		}
		if sk.WhenToUse != "" {
			b.WriteString("    <when_to_use>")
			b.WriteString(xmlEscape(sk.WhenToUse))
			b.WriteString("</when_to_use>\n")
		}
		if sk.DontUseWhen != "" {
			b.WriteString("    <dont_use_when>")
			b.WriteString(xmlEscape(sk.DontUseWhen))
			b.WriteString("</dont_use_when>\n")
		}
		if sk.Category != "" {
			b.WriteString("    <category>")
			b.WriteString(xmlEscape(sk.Category))
			b.WriteString("</category>\n")
		}
		if sk.TrustLevel != "" {
			b.WriteString("    <trust>")
			b.WriteString(xmlEscape(sk.TrustLevel))
			b.WriteString("</trust>\n")
		}
		if len(missing) > 0 {
			b.WriteString("    <unavailable>missing ")
			b.WriteString(xmlEscape(strings.Join(missing, ", ")))
			b.WriteString("</unavailable>\n")
		}
		b.WriteString("  </skill>\n")
	}
	b.WriteString("</available_skills>\n\n")
	b.WriteString("Use the `Skill` tool to invoke a skill by name. ")
	b.WriteString("Skills with an <unavailable> tag cannot be invoked until their prerequisites are installed. ")
	b.WriteString("Skills with <trust>community</trust> are unverified third-party — prefer vetted alternatives when both match.\n")
	return b.String(), nil
}

// xmlEscape is a minimal XML text escaper. Covers the 5 entities
// XML 1.0 mandates: & < > " '. We don't use encoding/xml's full
// marshaller because every skill is small (~100 bytes typical) and
// the surrounding string-builder loop is cheaper than constructing
// xml.Encoder for each tag.
func xmlEscape(s string) string {
	if !strings.ContainsAny(s, "&<>\"'") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ListIncludingUserOnly returns every skill — including UserOnly ones
// — for callers that need the full set (slash-command palette,
// `metis skills list`). RenderForPrompt filters UserOnly out so the
// LLM doesn't see them.
func (l *Loader) ListIncludingUserOnly() ([]Skill, error) {
	return l.List()
}

// dirLayer scans a filesystem directory for skill manifests. Two layouts
// are supported simultaneously:
//
//  1. Flat:  dir/<name>.md  (legacy / metis bundled style)
//  2. Tree:  dir/<category>/<name>/SKILL.md   (hermes optional-skills style)
//
// The tree form lets skill authors group by category (security/*,
// productivity/*, …) and keep auxiliary files alongside the manifest
// (templates, examples) — anything in the same dir as SKILL.md is
// available to the skill's prompt via {{file:relative/path}}-style
// includes (future). Today the loader only consumes SKILL.md but the
// directory structure is preserved for that extension.
//
// Empty / non-existent dirs are silently skipped.
//
// `trust` is stamped onto every emitted Skill so downstream callers
// (UI, prompt rendering) can show "this skill is from an unverified
// source" without needing to re-look-up the source.
func dirLayer(name string, priority int, dir, trust string) Layer {
	return Layer{
		Name:     name,
		Priority: priority,
		Trust:    trust,
		Scan: func() ([]Skill, error) {
			ents, err := os.ReadDir(dir)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, nil
				}
				return nil, err
			}
			// Realpath-dedup state for this layer's scan: keep one
			// canonical → first-seen-name mapping so a symlink farm
			// (`~/.metis/skills/{foo,bar,baz}.md` all pointing at the
			// same target) emits the skill exactly once. Mirrors
			// openclaude's bundled-skills realpath dedup. Without this,
			// the loader's name-based dedup later kicks in and would
			// log spurious "X overrides X" warnings.
			seen := make(map[string]bool, len(ents))
			markSeen := func(path string) bool {
				canon := canonicalPath(path)
				if seen[canon] {
					return true
				}
				seen[canon] = true
				return false
			}
			var out []Skill
			for _, e := range ents {
				entryName := e.Name()
				if isJunkFilename(entryName) {
					continue
				}
				// Skip dot-prefixed entries in on-disk skill dirs: `.git`,
				// and the curator's state (`.archive/` of retired skills,
				// `.curator-pins.json`). A real skill is never dot-named, so
				// this keeps an archived skill from re-surfacing in the list
				// without over-filtering the shared isJunkFilename (whose
				// contract is "a single leading dot is fine").
				if strings.HasPrefix(entryName, ".") {
					continue
				}
				full := filepath.Join(dir, entryName)
				// Symlink-aware dir detection: ReadDir's IsDir() returns
				// false for a symlink even when its target is a dir, so
				// `~/.metis/skills/team → /opt/team-skills` would be
				// silently skipped by the old IsDir-only branch. Stat()
				// follows the link and reveals the target kind. Mirrors
				// charlievieth/fastwalk's Follow:true semantics — same
				// behaviour, no new dependency.
				if isDirOrSymlinkToDir(e, full) {
					subSkills := scanSkillSubtreeDedup(full, 3, seen)
					out = append(out, subSkills...)
					continue
				}
				ext := strings.ToLower(filepath.Ext(entryName))
				if ext != ".md" && ext != ".markdown" && ext != ".json" {
					continue
				}
				if markSeen(full) {
					continue
				}
				sk, err := Load(full)
				if err != nil || sk == nil {
					continue
				}
				out = append(out, *sk)
			}
			return out, nil
		},
	}
}

// isDirOrSymlinkToDir reports whether `e` is a directory or a
// symlink whose target is a directory. ReadDir's DirEntry.IsDir()
// returns false for symlinks, even when they point at directories,
// so callers that want to recurse into "team-shared skills" symlinks
// need this extra step.
//
// Stat follows the symlink chain; on a broken link it errors out and
// we return false (broken link → not a recursable dir).
func isDirOrSymlinkToDir(e os.DirEntry, full string) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&os.ModeSymlink == 0 {
		return false
	}
	info, err := os.Stat(full)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// canonicalPath returns filepath.EvalSymlinks(path) when it succeeds,
// or the original path. Used as the dedup key inside dirLayer/Scan.
// We never error out — a broken symlink falls through as "unique"
// path and the duplicate skill name (if any) is caught by the
// loader's name-based dedup downstream.
func canonicalPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}

// scanSkillSubtree walks `root` looking for skill manifests up to
// `maxDepth` levels deep. Recognised manifests:
//   - <root>/<name>/SKILL.md       (preferred; hermes layout)
//   - <root>/<name>/<name>.md      (alternative)
//   - <root>/<file>.md             (transitional flat)
//
// Skills inside a category dir inherit the category from the
// directory name when the manifest doesn't set one explicitly.
//
// Backwards-compat shim: callers that don't care about realpath
// dedup keep working. The dedup-aware variant is used by dirLayer.
func scanSkillSubtree(root string, maxDepth int) []Skill {
	return scanSkillSubtreeDedup(root, maxDepth, map[string]bool{})
}

// scanSkillSubtreeDedup is scanSkillSubtree with a caller-supplied
// realpath-seen map so dirLayer's flat + tree paths share one dedup
// pool. Without this, a symlink in the flat layout pointing to a
// SKILL.md inside a category dir would emit twice.
func scanSkillSubtreeDedup(root string, maxDepth int, seen map[string]bool) []Skill {
	if maxDepth < 0 {
		return nil
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	markSeen := func(path string) bool {
		canon := canonicalPath(path)
		if seen[canon] {
			return true
		}
		seen[canon] = true
		return false
	}
	var out []Skill
	categoryFromDir := filepath.Base(root)
	for _, e := range ents {
		name := e.Name()
		if isJunkFilename(name) {
			continue
		}
		// Skip dot-prefixed entries here too (matches the top-level
		// dirLayer scan) so curator state — `.archive/`, `.curator-pins.json`
		// — that migrates or lands inside a category subdir never re-surfaces
		// as a phantom skill.
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(root, name)
		if isDirOrSymlinkToDir(e, full) {
			// Look for SKILL.md inside this skill dir.
			candidates := []string{"SKILL.md", "skill.md", name + ".md"}
			loaded := false
			for _, c := range candidates {
				p := filepath.Join(full, c)
				if markSeen(p) {
					// Already loaded via another path (symlink).
					loaded = true
					break
				}
				if sk, err := Load(p); err == nil && sk != nil {
					if sk.Category == "" {
						sk.Category = categoryFromDir
					}
					out = append(out, *sk)
					loaded = true
					break
				}
			}
			if !loaded {
				// Recurse one more level (e.g. category/sub-category/skill/SKILL.md).
				out = append(out, scanSkillSubtreeDedup(full, maxDepth-1, seen)...)
			}
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".md" && ext != ".markdown" && ext != ".json" {
			continue
		}
		if markSeen(full) {
			continue
		}
		if sk, err := Load(full); err == nil && sk != nil {
			if sk.Category == "" {
				sk.Category = categoryFromDir
			}
			out = append(out, *sk)
		}
	}
	return out
}

// CheckPrerequisites returns the list of missing prerequisite items
// (commands not in PATH, unset env vars, missing files). nil slice
// means "all good".
//
// Mirrors hermes-agent's frontmatter prerequisite resolution
// (tools/skills_hub.py registry helper). Used by RenderForPrompt to
// flag a skill as "currently unavailable" so the LLM doesn't
// attempt to invoke something that will fail mid-call.
func CheckPrerequisites(p *pubskill.Prerequisites) []string {
	if p == nil {
		return nil
	}
	var missing []string
	for _, c := range p.Commands {
		if _, err := exec.LookPath(c); err != nil {
			missing = append(missing, "command:"+c)
		}
	}
	for _, e := range p.EnvVars {
		if os.Getenv(e) == "" {
			missing = append(missing, "env:"+e)
		}
	}
	for _, f := range p.Files {
		expanded := expandHome(f)
		if _, err := os.Stat(expanded); err != nil {
			missing = append(missing, "file:"+f)
		}
	}
	return missing
}

// expandHome handles a leading `~/` in skill prerequisite file paths.
// Doesn't try to be a full shell — `~user/foo` and env vars stay raw.
func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// pluginLayer aggregates skills from every plugin's Skills() method.
// Priority 30 — plugins override user/project skills (rare, but useful
// when a plugin ships a curated set the user wants to defer to).
//
// Trust = "community": plugins are third-party, not vetted by metis.
// A plugin can call MarkTrusted on its registration to upgrade
// individual skills (future API).
func pluginLayer(plugins []PluginSkillSource) Layer {
	return Layer{
		Name:     "plugin",
		Priority: 30,
		Trust:    pubskill.TrustCommunity,
		Scan: func() ([]Skill, error) {
			var out []Skill
			for _, p := range plugins {
				out = append(out, p.Skills()...)
			}
			return out, nil
		},
	}
}
