package tui

import (
	"sort"

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
	"github.com/Ricardo-M-L/metis/internal/version"
)

// buildHelpTabs assembles the three-tab content for /help, mirroring
// claude-code's general / commands / custom-commands split (images
// #7-9 in the user TUI feedback). Constructed at slash-time so it
// reflects the live REPLCommandRegistry + skill loader state.
func (m *Model) buildHelpTabs() []screen.HelpTab {
	return []screen.HelpTab{
		{Name: "general", Body: m.helpGeneralRows()},
		{Name: "commands", Body: m.helpCommandsRows()},
		{Name: "custom-commands", Body: m.helpCustomCommandsRows()},
	}
}

// helpGeneralRows — the tab the user lands on by default. Mirrors
// claude-code's "general" tab: short product blurb + key shortcuts.
func (m *Model) helpGeneralRows() []screen.HelpRow {
	return []screen.HelpRow{
		{Value: "metis understands your codebase, makes edits with your"},
		{Value: "permission, and executes commands — right from the terminal."},
		{},
		{Heading: "Shortcuts"},
		{Key: "!", Value: "shell mode (`!ls -la`)"},
		{Key: "/", Value: "slash command palette"},
		{Key: "@", Value: "@-mention a file path"},
		{Key: "&", Value: "background a long task"},
		{Key: "/btw", Value: "side question without disturbing main turn"},
		{},
		{Heading: "Editing"},
		{Key: "Alt+Enter / Ctrl+J", Value: "insert newline (multi-line prompt)"},
		{Key: "Esc", Value: "dismiss palette / modal / pending state"},
		{Key: "Esc Esc", Value: "clear input completely"},
		{Key: "Tab", Value: "autocomplete slash / @-mention"},
		{},
		{Heading: "Navigation"},
		{Key: "Ctrl+R", Value: "history search overlay"},
		{Key: "Ctrl+T", Value: "toggle task panel"},
		{Key: "Ctrl+O", Value: "toggle expanded tool output"},
		{Key: "Ctrl+L", Value: "show available models"},
		{Key: "PgUp/PgDn", Value: "scroll viewport"},
		{Key: "Home/End", Value: "jump top/bottom"},
		{},
		{Heading: "Session"},
		{Key: "Ctrl+C", Value: "exit (idle) / cancel turn (active)"},
		{Key: "Ctrl+D", Value: "quit"},
		{Key: "Ctrl+P", Value: "session picker"},
		{Key: "Ctrl+V", Value: "paste from clipboard (image/text)"},
		{Key: "Ctrl+Y", Value: "yank last assistant reply"},
		{Key: "Mouse click", Value: "copy that chat row to clipboard"},
		{Key: "Shift+Tab", Value: "cycle permission mode"},
	}
}

// helpCommandsRows — every registered REPL command sorted alphabetically.
// Long list, scrollable. Mirrors claude-code's "commands" tab (image #8).
func (m *Model) helpCommandsRows() []screen.HelpRow {
	all := m.cmds.All()
	// Dedup by canonical name (aliases share the underlying entry).
	seen := map[string]bool{}
	type entry struct{ name, desc string }
	pairs := make([]entry, 0, len(all))
	for _, c := range all {
		if seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		pairs = append(pairs, entry{name: c.Name, desc: c.Description})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].name < pairs[j].name })
	rows := []screen.HelpRow{
		{Heading: "Built-in commands"},
		{Value: "Type any of these or use `/` + Tab to autocomplete."},
		{},
	}
	for _, p := range pairs {
		rows = append(rows, screen.HelpRow{Key: "/" + p.name, Value: p.desc})
	}
	rows = append(rows,
		screen.HelpRow{},
		screen.HelpRow{Value: "Or type anything else to chat with the LLM."},
	)
	return rows
}

// helpCustomCommandsRows — bundled + user-installed skills surface as
// "custom commands" to mirror claude-code's "custom-commands" tab
// (image #9). Each skill becomes a /<name> the user can invoke via
// the Skill tool.
func (m *Model) helpCustomCommandsRows() []screen.HelpRow {
	rows := []screen.HelpRow{
		{Heading: "Custom commands (skills)"},
		{Value: "Bundled + user-installed skills available via the Skill tool."},
		{Value: "Drop a SKILL.md under ~/.metis/skills/ to add your own."},
		{},
	}
	loader := skills.NewLoader(m.skillDir, "", nil)
	list, err := loader.List()
	if err != nil || len(list) == 0 {
		rows = append(rows, screen.HelpRow{Value: "(no skills loaded)"})
		return rows
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	for _, sk := range list {
		desc := sk.Description
		if desc == "" {
			desc = "(no description)"
		}
		rows = append(rows, screen.HelpRow{Key: "/" + sk.Name, Value: desc})
	}
	return rows
}

// versionLabel returns the version string used in the help screen header.
func versionLabel() string {
	return "v" + version.Version
}
