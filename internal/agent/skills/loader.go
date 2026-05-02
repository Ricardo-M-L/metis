package skills

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Layer is one priority-tagged source the multi-source loader scans. The
// `Scan` callback returns the skills the layer currently has; `Priority`
// determines who wins when two layers contribute a skill of the same name
// (higher wins). Conventional priority floor → ceiling:
//
//	bundled(0) < user(10) < project(20) < plugin(30) < mcp(40)
type Layer struct {
	Name     string
	Priority int
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

	mu    sync.RWMutex
	cache []Skill
	dirty bool
}

// NewLoader builds the standard 4-layer loader (bundled / user / project /
// plugin). The MCP layer is omitted when mcp is nil — kept as a separate
// constructor option so unit tests don't need an MCP client.
//
// `userDir` is typically `~/.metis/skills`; `projectDir` is `$PWD/.metis/skills`
// (resolved at construct time). Pass empty string to disable a layer.
func NewLoader(userDir, projectDir string, plugins []PluginSkillSource) *Loader {
	layers := []Layer{
		bundledLayer(),
	}
	if userDir != "" {
		layers = append(layers, dirLayer("user", 10, userDir))
	}
	if projectDir != "" {
		layers = append(layers, dirLayer("project", 20, projectDir))
	}
	if len(plugins) > 0 {
		layers = append(layers, pluginLayer(plugins))
	}
	sort.Slice(layers, func(i, j int) bool { return layers[i].Priority < layers[j].Priority })
	return &Loader{
		Layers: layers,
		Logger: func(string, ...any) {},
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
// available skill. Output mirrors Store.RenderForPrompt's format so the
// agent loop can swap implementations transparently.
func (l *Loader) RenderForPrompt() (string, error) {
	skills, err := l.List()
	if err != nil || len(skills) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString("## Available Skills\n\n")
	for _, sk := range skills {
		b.WriteString("- **")
		b.WriteString(sk.Name)
		b.WriteString("**")
		if sk.Description != "" {
			b.WriteString(": ")
			b.WriteString(sk.Description)
		}
		if sk.WhenToUse != "" {
			b.WriteString(" (use when: ")
			b.WriteString(sk.WhenToUse)
			b.WriteString(")")
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nUse the `Skill` tool to invoke a skill by name.\n")
	return b.String(), nil
}

// dirLayer scans a filesystem directory for *.md and *.json skills. Empty
// dir or non-existent dir is silently skipped (returns []) so a fresh
// install with no user skills doesn't error.
func dirLayer(name string, priority int, dir string) Layer {
	return Layer{
		Name:     name,
		Priority: priority,
		Scan: func() ([]Skill, error) {
			ents, err := os.ReadDir(dir)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, nil
				}
				return nil, err
			}
			var out []Skill
			for _, e := range ents {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				if isJunkFilename(name) {
					continue
				}
				ext := strings.ToLower(filepath.Ext(name))
				if ext != ".md" && ext != ".markdown" && ext != ".json" {
					continue
				}
				sk, err := Load(filepath.Join(dir, name))
				if err != nil || sk == nil {
					continue
				}
				out = append(out, *sk)
			}
			return out, nil
		},
	}
}

// pluginLayer aggregates skills from every plugin's Skills() method.
// Priority 30 — plugins override user/project skills (rare, but useful
// when a plugin ships a curated set the user wants to defer to).
func pluginLayer(plugins []PluginSkillSource) Layer {
	return Layer{
		Name:     "plugin",
		Priority: 30,
		Scan: func() ([]Skill, error) {
			var out []Skill
			for _, p := range plugins {
				out = append(out, p.Skills()...)
			}
			return out, nil
		},
	}
}
