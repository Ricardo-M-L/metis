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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
)

// handleSkillInfo renders one skill's full manifest in the chat. Unlike
// the singular `/skill list` (just names), info surfaces description,
// when_to_use, allowed_tools, version, source — the same fields the
// `metis skills info` CLI prints, but inside the chat surface.
func (r *REPL) handleSkillInfo(name string) string {
	if r.skillDir == "" {
		return "(skills directory not configured)"
	}
	store := skills.NewStore(r.skillDir)
	sk, err := store.Get(name)
	if err != nil {
		return "(no skill named: " + name + ")"
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

// handleSkillEdit opens the skill's JSON manifest in $EDITOR. Re-loads
// after save to surface decode errors before the next system prompt
// build silently drops the manifest.
func (r *REPL) handleSkillEdit(name string) string {
	if r.skillDir == "" {
		return "(skills directory not configured)"
	}
	store := skills.NewStore(r.skillDir)
	if _, err := store.Get(name); err != nil {
		return "(no skill named: " + name + ")"
	}
	path := filepath.Join(r.skillDir, sanitize(name)+".json")
	editor := pickEditor()
	cmd := exec.Command(editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "skill edit: " + err.Error()
	}
	if _, err := store.Get(name); err != nil {
		return "skill edit: file saved but parse failed — fix it before next launch:\n  " + err.Error()
	}
	return "(skill " + name + " saved)"
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
	store := skills.NewStore(r.skillDir)
	sk, err := store.Get(name)
	if err != nil {
		return "(no skill named: " + name + ")"
	}
	if sk.Disabled == disabled {
		state := "enabled"
		if disabled {
			state = "disabled"
		}
		return "(skill " + name + " already " + state + ")"
	}
	sk.Disabled = disabled
	if err := store.Save(sk); err != nil {
		return "skill: save: " + err.Error()
	}
	if disabled {
		return "(disabled skill: " + name + " — pulled from system prompt next turn)"
	}
	return "(enabled skill: " + name + " — back in system prompt next turn)"
}

// handleSkillCreate scaffolds a new skill at ~/.metis/skills/<name>.json
// with sensible defaults the user fills in. Refuses to overwrite an
// existing manifest — `/skills edit` is the path for that. Sanitization
// matches the rest of the skill family ('/' '\' '.' stripped).
func (r *REPL) handleSkillCreate(name string) string {
	if r.skillDir == "" {
		return "(skills directory not configured)"
	}
	clean := sanitize(name)
	if clean == "" {
		return "(skill: empty name after sanitization)"
	}
	path := filepath.Join(r.skillDir, clean+".json")
	if _, err := os.Stat(path); err == nil {
		return "(skill already exists: " + clean + " — `/skills edit " + clean + "` to modify)"
	}
	store := skills.NewStore(r.skillDir)
	sk := &skills.Skill{
		Name:        clean,
		Description: "TODO: one-sentence description that helps the LLM pick this skill",
		Category:    "custom",
		Prompt:      "TODO: write the actual skill prompt here. The LLM treats this as instructions when the skill is invoked.",
		WhenToUse:   "TODO: when should this skill activate? (free-form, picked up by the LLM)",
	}
	if err := store.Save(sk); err != nil {
		return "skill: create: " + err.Error()
	}
	return "(created skill: " + clean + " · " + path + " — `/skills edit " + clean + "` to fill in TODOs)"
}

// listSkillsForSearch is a small helper that builds (name, description)
// pairs for local fuzzy-match. Pulled out so the search handler can
// reuse the same enumeration logic the LLM-side prompt builder runs
// (everything in r.skillDir, .json only).
func (r *REPL) listSkillsForSearch() []skills.Skill {
	if r.skillDir == "" {
		return nil
	}
	store := skills.NewStore(r.skillDir)
	all, err := store.List()
	if err != nil {
		return nil
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all
}

// handleSkillSearchLocal does a fuzzy match against the LOCAL store.
// The existing handleSkillSearch on r hits GitHub; that's the right
// behaviour for `/skill search` (discover new skills), but `/skills
// search` should surface what's already installed — same name, same
// behaviour as the local `metis skills list | grep <q>` pattern.
func (r *REPL) handleSkillSearchLocal(query string) string {
	if query == "" {
		return "usage: /skills search <query>"
	}
	all := r.listSkillsForSearch()
	if len(all) == 0 {
		return "(no skills installed)"
	}
	q := strings.ToLower(query)
	var hits []skills.Skill
	for _, sk := range all {
		hay := strings.ToLower(sk.Name + " " + sk.Description + " " + sk.Category + " " + strings.Join(sk.Tags, " "))
		if strings.Contains(hay, q) {
			hits = append(hits, sk)
		}
	}
	if len(hits) == 0 {
		return "(no installed skills match: " + query + " — `/skill search " + query + "` searches github)"
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
		// No args — preserve the old "render full installed list" behavior.
		return renderSkillsList(r.skillDir)
	}
	parts := strings.SplitN(args, " ", 2)
	sub := strings.ToLower(strings.TrimSpace(parts[0]))
	rest := ""
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}
	switch sub {
	case "list", "ls":
		return renderSkillsList(r.skillDir)
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
		// Local search by default. `/skill search` (singular) hits github.
		return r.handleSkillSearchLocal(rest)
	}
	return "skills: unknown '" + sub + "'. usage: /skills list | install <n> | remove <n> | info <n> |\n" +
		"  edit <n> | enable <n> | disable <n> | create <n> | search <q>"
}
