package tui

// cmd_skills_extra.go — Phase A: subcommands beyond list/install/uninstall/
// search. Handles info, edit, enable, disable, create. Each helper is a
// method on *REPL so it can reach r.skillDir without re-parsing config.
//
// The /skill (singular) command historically only knew list/install/
// uninstall/search; we extend its switch in commands.go and register
// /skills (plural) as an alias-with-superset, mirroring claude-code's
// "/skills <subcommand>" convention.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
	"gopkg.in/yaml.v3"
)

const canonicalSkillManifest = "SKILL.md"

var errManagedSkillNotFound = errors.New("managed skill not found")

// validManagedSkillName accepts the Agent Skills directory-name convention
// while refusing path traversal, whitespace, and shell-shaped input. Install
// planning intentionally does not use this validator: it preserves the exact
// name the user typed so the existing planner can flag typos/ambiguity.
func validManagedSkillName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("name is required")
	}
	for _, ch := range name {
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '-' || ch == '_' {
			continue
		}
		return "", fmt.Errorf("invalid name %q (use letters, numbers, '-' or '_')", raw)
	}
	return name, nil
}

func pathInside(root, path string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// userSkillDeclarations reads the configured user layer before availability
// filters are applied. This is important for management: a disabled skill is
// absent from Loader.List/Get but must still be discoverable by /skills enable.
func userSkillDeclarations(root string) ([]skills.Skill, error) {
	if root == "" {
		return nil, errors.New("skills directory not configured")
	}
	loader := skills.NewLoader(root, "", nil)
	declared, err := loader.ListDeclared()
	if err != nil {
		return nil, err
	}
	out := make([]skills.Skill, 0, len(declared))
	for _, sk := range declared {
		if sk.LocalPath != "" && pathInside(root, sk.LocalPath) {
			out = append(out, sk)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// locateManagedSkill returns only a manifest in r.skillDir. Bundled, project,
// plugin, optional, and universal skills are intentionally read-only from this
// command family; their own package manager/source owns their lifecycle.
func locateManagedSkill(root, rawName string) (string, error) {
	name := strings.TrimSpace(rawName)
	if declared, err := userSkillDeclarations(root); err == nil {
		for _, sk := range declared {
			if sk.Name == name || strings.EqualFold(sk.Name, name) {
				return sk.LocalPath, nil
			}
		}
	}

	clean, err := validManagedSkillName(name)
	if err != nil {
		return "", err
	}
	// Exact canonical paths remain editable/removable even when the YAML is
	// temporarily malformed (and therefore omitted by ListDeclared).
	candidates := []string{
		filepath.Join(root, clean, canonicalSkillManifest),
		filepath.Join(root, clean, "skill.md"),
		filepath.Join(root, clean, clean+".md"),
		filepath.Join(root, clean+".md"), // transitional flat manifests
		filepath.Join(root, clean+".markdown"),
	}
	for _, path := range candidates {
		if !pathInside(root, path) {
			continue
		}
		if st, statErr := os.Stat(path); statErr == nil && !st.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("%w: %s", errManagedSkillNotFound, name)
}

func (r *REPL) invalidateSkillCatalog() {
	if loader := skillCatalogLoader(r.Loop, r.skillDir); loader != nil {
		loader.Invalidate()
	}
}

// handleSkillInfo renders one skill's full manifest in the chat. Unlike
// the singular `/skill list` (just names), info surfaces description,
// when_to_use, allowed_tools, version, source — the same fields the
// `metis skills info` CLI prints, but inside the chat surface.
func (r *REPL) handleSkillInfo(name string) string {
	if r.skillDir == "" {
		return "(skills directory not configured)"
	}
	var sk *skills.Skill
	if path, err := locateManagedSkill(r.skillDir, name); err == nil {
		loaded, loadErr := skills.Load(path)
		if loadErr != nil {
			return "skill info: " + loadErr.Error()
		}
		sk = loaded
	} else if !errors.Is(err, errManagedSkillNotFound) {
		return "skill info: " + err.Error()
	} else {
		// Info remains useful for immutable bundled/project/plugin skills.
		loader := skillCatalogLoader(r.Loop, r.skillDir)
		loader.Invalidate()
		loaded, loadErr := loader.Get(strings.TrimSpace(name))
		if loadErr != nil {
			return "skill info: " + loadErr.Error()
		}
		sk = loaded
	}
	if sk == nil {
		return "(no skill named: " + strings.TrimSpace(name) + ")"
	}
	rows := []infoRow{
		{Key: "name", Value: sk.Name},
	}
	if sk.Description != "" {
		rows = append(rows, infoRow{Key: "description", Value: sk.Description})
	}
	if sk.Category != "" {
		rows = append(rows, infoRow{Key: "category", Value: sk.Category})
	}
	if sk.Version != "" {
		rows = append(rows, infoRow{Key: "version", Value: sk.Version})
	}
	if sk.Source != "" {
		rows = append(rows, infoRow{Key: "source", Value: sk.Source})
	}
	if sk.WhenToUse != "" {
		rows = append(rows, infoRow{Key: "when to use", Value: sk.WhenToUse})
	}
	if sk.DontUseWhen != "" {
		rows = append(rows, infoRow{Key: "don't use when", Value: sk.DontUseWhen})
	}
	if len(sk.Tags) > 0 {
		rows = append(rows, infoRow{Key: "tags", Value: strings.Join(sk.Tags, ", ")})
	}
	if len(sk.AllowedTools) > 0 {
		rows = append(rows, infoRow{Key: "allowed_tools", Value: strings.Join(sk.AllowedTools, ", ")})
	}
	if len(sk.ActivateOn) > 0 {
		rows = append(rows, infoRow{Key: "activate_on", Value: strings.Join(sk.ActivateOn, ", ")})
	}
	state := "enabled"
	if sk.Disabled {
		state = "disabled"
	}
	rows = append(rows, infoRow{Key: "state", Value: state})
	if sk.UserOnly {
		rows = append(rows, infoRow{Key: "scope", Value: "user-only (LLM cannot self-invoke)"})
	}
	if sk.LocalPath != "" {
		rows = append(rows, infoRow{Key: "manifest", Value: sk.LocalPath})
	}
	rows = append(rows, infoRow{Key: "uses", Value: fmt.Sprintf("%d", sk.Uses)})
	if sk.ContentHash != "" {
		short := sk.ContentHash
		if len(short) > 12 {
			short = short[:12] + "…"
		}
		rows = append(rows, infoRow{Key: "hash", Value: short})
	}
	if sk.Prompt != "" {
		preview := firstNonEmptyLine(sk.Prompt)
		if len(preview) > 80 {
			preview = preview[:80] + "…"
		}
		rows = append(rows, infoRow{Key: "prompt", Value: preview, Hint: "/skills edit " + sk.Name + " for full body"})
	}
	return renderInfoBox("Skill · "+sk.Name, rows)
}

// handleSkillEdit opens the user-owned SKILL.md in $EDITOR. Re-loads
// after save to surface decode errors before the next system prompt
// build silently drops the manifest.
func (r *REPL) handleSkillEdit(name string) string {
	if r.skillDir == "" {
		return "(skills directory not configured)"
	}
	path, err := locateManagedSkill(r.skillDir, name)
	if err != nil {
		if errors.Is(err, errManagedSkillNotFound) {
			return "(no locally managed skill named: " + strings.TrimSpace(name) + " — bundled/project/plugin skills are read-only here)"
		}
		return "skill edit: " + err.Error()
	}
	editor := pickEditor()
	cmd := exec.Command(editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "skill edit: " + err.Error()
	}
	if _, err := skills.Load(path); err != nil {
		return "skill edit: file saved but parse failed — fix it before next launch:\n  " + err.Error()
	}
	r.invalidateSkillCatalog()
	return "(skill " + strings.TrimSpace(name) + " saved · " + path + ")"
}

// handleSkillEnable / handleSkillDisable flip the Disabled flag on the
// manifest and persist. Skills with Disabled=true are filtered out of
// the system prompt by the loader (see internal/agent/skills/loader.go).
// Idempotent — toggling an already-enabled skill returns "(no change)".
func (r *REPL) handleSkillEnable(name string) string {
	return r.setSkillState(name, false)
}

func (r *REPL) handleSkillDisable(name string) string {
	return r.setSkillState(name, true)
}

func (r *REPL) setSkillState(name string, disabled bool) string {
	if r.skillDir == "" {
		return "(skills directory not configured)"
	}
	path, err := locateManagedSkill(r.skillDir, name)
	if err != nil {
		if errors.Is(err, errManagedSkillNotFound) {
			return "(no locally managed skill named: " + strings.TrimSpace(name) + ")"
		}
		return "skill: " + err.Error()
	}
	sk, err := skills.Load(path)
	if err != nil {
		return "skill: parse: " + err.Error()
	}
	if sk.Disabled == disabled {
		state := "enabled"
		if disabled {
			state = "disabled"
		}
		return "(skill " + strings.TrimSpace(name) + " already " + state + ")"
	}
	clean, err := validManagedSkillName(name)
	if err != nil {
		return "skill: " + err.Error()
	}
	if err := rewriteSkillDisabled(path, clean, disabled); err != nil {
		return "skill: save: " + err.Error()
	}
	r.invalidateSkillCatalog()
	if disabled {
		return "(disabled skill: " + clean + " — removed from the live catalog next refresh)"
	}
	return "(enabled skill: " + clean + " — eligible for the live catalog next refresh)"
}

// handleSkillCreate scaffolds the canonical Agent Skills layout:
// ~/.metis/skills/<name>/SKILL.md. The Markdown body is deliberately
// non-empty so creation can never produce the old inert JSON manifest whose
// prompt was "". Refuses to overwrite an existing directory or manifest.
func (r *REPL) handleSkillCreate(name string) string {
	if r.skillDir == "" {
		return "(skills directory not configured)"
	}
	clean, err := validManagedSkillName(name)
	if err != nil {
		return "skill: create: " + err.Error()
	}
	dir := filepath.Join(r.skillDir, clean)
	path := filepath.Join(dir, canonicalSkillManifest)
	if _, err := os.Lstat(dir); err == nil {
		return "(skill already exists: " + clean + " — `/skills edit " + clean + "` to modify)"
	} else if !os.IsNotExist(err) {
		return "skill: create: " + err.Error()
	}
	if err := os.MkdirAll(r.skillDir, 0o755); err != nil {
		return "skill: create: " + err.Error()
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "skill: create: " + err.Error()
	}
	meta := &skills.Skill{
		Name:        clean,
		Description: "TODO: one-sentence description that helps the LLM pick this skill",
		Category:    "custom",
		WhenToUse:   "TODO: describe the tasks that should activate this skill",
	}
	header, err := yaml.Marshal(meta)
	if err != nil {
		_ = os.Remove(dir)
		return "skill: create: " + err.Error()
	}
	body := fmt.Sprintf("# %s\n\nTODO: write the concrete procedure for this skill. Keep the steps executable and scoped.\n", clean)
	manifest := "---\n" + strings.TrimSpace(string(header)) + "\n---\n\n" + body
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		_ = os.Remove(dir)
		return "skill: create: " + err.Error()
	}
	r.invalidateSkillCatalog()
	return "(created skill: " + clean + " · " + path + " — `/skills edit " + clean + "` to fill in TODOs)"
}

func splitSkillFrontmatterForEdit(data []byte) (header, body []byte, ok bool) {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return nil, data, false
	}
	rest := data[len("---\n"):]
	if idx := bytes.Index(rest, []byte("\n---\n")); idx >= 0 {
		return rest[:idx], rest[idx+len("\n---\n"):], true
	}
	if idx := bytes.Index(rest, []byte("\n---")); idx >= 0 && idx+len("\n---") == len(rest) {
		return rest[:idx], nil, true
	}
	return nil, data, false
}

func skillYAMLMapping(header []byte) (*yaml.Node, error) {
	doc := &yaml.Node{}
	if len(bytes.TrimSpace(header)) > 0 {
		if err := yaml.Unmarshal(header, doc); err != nil {
			return nil, err
		}
	}
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("frontmatter must be a YAML mapping")
	}
	return doc, nil
}

func setYAMLScalar(mapping *yaml.Node, key, value, tag string, remove bool) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		if remove {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
		mapping.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
		return
	}
	if remove {
		return
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value},
	)
}

func yamlMappingHasKey(mapping *yaml.Node, key string) bool {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return true
		}
	}
	return false
}

// rewriteSkillDisabled preserves the Markdown body and unknown frontmatter
// keys. Enabling removes the field entirely so ordinary manifests stay clean.
// A frontmatter-less canonical file gains a name at the same time; otherwise a
// file named SKILL.md would be loaded under the useless default name "SKILL".
func rewriteSkillDisabled(path, fallbackName string, disabled bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	header, body, _ := splitSkillFrontmatterForEdit(data)
	doc, err := skillYAMLMapping(header)
	if err != nil {
		return err
	}
	mapping := doc.Content[0]
	if !yamlMappingHasKey(mapping, "name") {
		setYAMLScalar(mapping, "name", fallbackName, "!!str", false)
	}
	setYAMLScalar(mapping, "disabled", "true", "!!bool", !disabled)
	encoded, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(bytes.TrimSpace(encoded))
	out.WriteString("\n---\n")
	if trimmedBody := bytes.TrimLeft(body, "\n"); len(trimmedBody) > 0 {
		out.WriteByte('\n')
		out.Write(trimmedBody)
		if trimmedBody[len(trimmedBody)-1] != '\n' {
			out.WriteByte('\n')
		}
	}
	mode := os.FileMode(0o644)
	if st, statErr := os.Stat(path); statErr == nil {
		mode = st.Mode().Perm()
	}
	return os.WriteFile(path, out.Bytes(), mode)
}

func (r *REPL) removeManagedSkill(name string) string {
	if r.skillDir == "" {
		return "(skills directory not configured)"
	}
	path, err := locateManagedSkill(r.skillDir, name)
	if err != nil {
		if errors.Is(err, errManagedSkillNotFound) {
			return "(skill not found in configured user skills: " + strings.TrimSpace(name) + ")"
		}
		return "skill remove: " + err.Error()
	}
	target := path
	base := filepath.Base(path)
	dir := filepath.Dir(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if strings.EqualFold(base, canonicalSkillManifest) || strings.EqualFold(base, "skill.md") || strings.EqualFold(stem, filepath.Base(dir)) {
		target = dir // remove the skill's assets together with its manifest
	}
	if !pathInside(r.skillDir, target) {
		return "skill remove: refusing path outside configured skills directory"
	}
	if target == path {
		err = os.Remove(target)
	} else {
		err = os.RemoveAll(target)
	}
	if err != nil {
		return "skill remove: " + err.Error()
	}
	r.invalidateSkillCatalog()
	return "removed skill: " + strings.TrimSpace(name) + " · " + target
}

// skillInstallPlan delegates to the same read-only planner the live Skill tool
// uses. It never downloads arbitrary GitHub content or runs a shell command
// from the Bubble Tea update loop; the user gets one vetted lifecycle or one
// bounded registry-discovery command to review and run explicitly.
func (r *REPL) skillInstallPlan(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "usage: /skills install <name>"
	}
	planner := builtin.NewSkill(nil, skillCatalogLoader(r.Loop, r.skillDir), r.skillDir)
	result, err := planner.Execute(context.Background(), map[string]any{
		"action": "plan_install",
		"names":  []string{name},
	})
	if err != nil {
		return "skill install plan: " + err.Error()
	}
	if result == nil || strings.TrimSpace(result.Output) == "" {
		return "skill install plan: planner returned no result"
	}
	prefix := "No files were downloaded or installed by this TUI command. Review the exact lifecycle below, run it explicitly, then use `/skills list` to verify.\n\n"
	return prefix + strings.TrimSpace(result.Output)
}

type managedSkillItem struct {
	Skill skills.Skill
	State string
}

func (r *REPL) managedSkillItems() []managedSkillItem {
	available, _ := loadSkillCatalog(r.Loop, r.skillDir)
	items := make(map[string]managedSkillItem, len(available))
	for _, sk := range available {
		items[sk.Name] = managedSkillItem{Skill: sk, State: "available"}
	}
	declared, _ := userSkillDeclarations(r.skillDir)
	for _, sk := range declared {
		if _, ok := items[sk.Name]; ok {
			continue
		}
		state := "unavailable"
		if sk.Disabled {
			state = "disabled"
		}
		items[sk.Name] = managedSkillItem{Skill: sk, State: state}
	}
	out := make([]managedSkillItem, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Skill.Name < out[j].Skill.Name })
	return out
}

func (r *REPL) renderManagedSkillsList() string {
	items := r.managedSkillItems()
	if len(items) == 0 {
		return "(no skills available)"
	}
	rows := make([]infoRow, 0, len(items))
	unavailable := 0
	for _, item := range items {
		desc := item.Skill.Description
		if desc == "" {
			desc = "(no description)"
		}
		hint := ""
		if item.State != "available" {
			hint = item.State
			unavailable++
		}
		rows = append(rows, infoRow{Key: item.Skill.Name, Value: desc, Hint: hint})
	}
	title := fmt.Sprintf("Skills · %d available", len(items)-unavailable)
	if unavailable > 0 {
		title += fmt.Sprintf(" · %d disabled/unavailable", unavailable)
	}
	return renderInfoBox(title, rows)
}

// listSkillsForSearch is a small helper that builds (name, description)
// pairs for local fuzzy-match. Pulled out so the search handler can
// reuse the live catalog while still surfacing disabled user manifests.
func (r *REPL) listSkillsForSearch() []skills.Skill {
	items := r.managedSkillItems()
	all := make([]skills.Skill, 0, len(items))
	for _, item := range items {
		all = append(all, item.Skill)
	}
	return all
}

// handleSkillSearchLocal does a fuzzy match against the LOCAL store.
// A miss returns the repository's guarded install planner, which emits one
// official lifecycle or one `npx skills find` discovery step. It deliberately
// does not use the legacy unauthenticated GitHub JSON code search.
func (r *REPL) handleSkillSearchLocal(query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return "usage: /skills search <query>"
	}
	all := r.listSkillsForSearch()
	q := strings.ToLower(query)
	var hits []skills.Skill
	for _, sk := range all {
		hay := strings.ToLower(sk.Name + " " + sk.Description + " " + sk.Category + " " + strings.Join(sk.Tags, " "))
		if strings.Contains(hay, q) {
			hits = append(hits, sk)
		}
	}
	if len(hits) == 0 {
		return "No local skills match: " + query + ".\n\n" + r.skillInstallPlan(query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d local match(es):\n", len(hits))
	for _, sk := range hits {
		state := ""
		if sk.Disabled {
			state = " [disabled]"
		}
		desc := sk.Description
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&b, "  %s%s — %s\n", sk.Name, state, desc)
	}
	return strings.TrimRight(b.String(), "\n")
}

// cmdSkills now dispatches the same superset as /skill so users typing
// the plural form get the full subcommand surface (`/skills install`,
// `/skills info`, etc.) rather than just the list. The previous handler
// returned a flat list; we keep that as the no-arg path. /skill stays
// alongside as a singular alias because external docs reference it.
func cmdSkills(r *REPL, args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		// Bubble Tea intercepts bare /skills and opens the picker. This is the
		// non-TUI REPL fallback over the same modern catalog.
		return r.renderManagedSkillsList()
	}
	parts := strings.SplitN(args, " ", 2)
	sub := strings.ToLower(strings.TrimSpace(parts[0]))
	rest := ""
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}
	switch sub {
	case "list", "ls":
		return r.renderManagedSkillsList()
	case "install":
		if rest == "" {
			return "usage: /skills install <name>"
		}
		return r.handleSkillInstall(rest)
	case "remove", "rm", "uninstall":
		if rest == "" {
			return "usage: /skills remove <name>"
		}
		return r.handleSkillUninstall(rest)
	case "info", "show":
		if rest == "" {
			return "usage: /skills info <name>"
		}
		return r.handleSkillInfo(rest)
	case "edit":
		if rest == "" {
			return "usage: /skills edit <name>"
		}
		return r.handleSkillEdit(rest)
	case "enable":
		if rest == "" {
			return "usage: /skills enable <name>"
		}
		return r.handleSkillEnable(rest)
	case "disable":
		if rest == "" {
			return "usage: /skills disable <name>"
		}
		return r.handleSkillDisable(rest)
	case "create", "new":
		if rest == "" {
			return "usage: /skills create <name>"
		}
		return r.handleSkillCreate(rest)
	case "search":
		return r.handleSkillSearchLocal(rest)
	}
	return "skills: unknown '" + sub + "'. usage: /skills list | install <n> | remove <n> | info <n> |\n" +
		"  edit <n> | enable <n> | disable <n> | create <n> | search <q>"
}
