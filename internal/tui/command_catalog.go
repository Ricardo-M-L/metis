package tui

import (
	"sort"
	"strings"
	"unicode"

	"github.com/Ricardo-M-L/metis/internal/slash"
)

// CommandCatalogEntry is the effective discovery metadata shared by the TUI
// palette, /help, and the plain readline completer. Handlers deliberately stay
// in their existing registries during phase one of the migration.
type CommandCatalogEntry struct {
	Name         string
	Aliases      []string
	Description  string
	ArgumentHint string
	Visible      bool
	Enabled      bool
	Source       string
	Category     string

	// MatchedAlias is transient palette state. The effective catalog and help
	// rows leave it empty; matchCommands sets it when an alias supplied the
	// best match so the suggestion can explain why a differently named
	// canonical command appeared.
	MatchedAlias string
}

func replCatalogEntry(c REPLCommand) CommandCatalogEntry {
	return CommandCatalogEntry{
		Name: c.Name, Aliases: append([]string(nil), c.Aliases...),
		Description: c.Description, ArgumentHint: c.ArgumentHint,
		Visible: c.IsVisible(), Enabled: c.IsEnabled(),
		Source: c.Source, Category: c.Category,
	}
}

func slashCatalogEntry(c slash.Cmd) CommandCatalogEntry {
	return CommandCatalogEntry{
		Name: c.Name, Aliases: append([]string(nil), c.Aliases...),
		Description: c.Description, ArgumentHint: c.ArgumentHint,
		Visible: c.IsVisible(), Enabled: c.IsEnabled(),
		Source: c.Source, Category: c.Category,
	}
}

// effectiveCommandCatalog merges the two execution registries according to
// the same ownership rules used by dispatch. It also applies alias collision
// resolution once, so every discovery surface resolves an alias to one
// canonical row. Invisible and disabled registrations still claim their
// routed names, but are not advertised.
func effectiveCommandCatalog(repl *REPLCommandRegistry, sl *slash.Registry) []CommandCatalogEntry {
	claimed := make(map[string]struct{})
	out := make([]CommandCatalogEntry, 0, 96)

	appendEntry := func(entry CommandCatalogEntry) {
		canonical := strings.ToLower(entry.Name)
		if canonical == "" {
			return
		}
		if _, exists := claimed[canonical]; exists {
			return
		}
		claimed[canonical] = struct{}{}
		aliases := make([]string, 0, len(entry.Aliases))
		for _, alias := range entry.Aliases {
			key := strings.ToLower(strings.TrimSpace(alias))
			if key == "" || key == canonical {
				continue
			}
			if _, exists := claimed[key]; exists {
				continue
			}
			claimed[key] = struct{}{}
			aliases = append(aliases, alias)
		}
		entry.Aliases = aliases
		if entry.Visible && entry.Enabled {
			out = append(out, entry)
		}
	}

	if repl != nil {
		for _, cmd := range repl.Catalog() {
			// A preferred slash handler must be reachable under both its
			// canonical name and compatibility aliases. Resolve, rather than
			// an exact-name scan, covers entries such as /diff-view -> /diff.
			if preferSlashCommand(cmd.Name) && sl != nil {
				if _, ok := sl.Resolve(cmd.Name); ok {
					continue
				}
			}
			appendEntry(replCatalogEntry(cmd))
		}
	}
	if sl != nil {
		for _, cmd := range sl.Catalog() {
			appendEntry(slashCatalogEntry(cmd))
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		leftTier, rightTier := commandCatalogTier(out[i]), commandCatalogTier(out[j])
		if leftTier != rightTier {
			return leftTier < rightTier
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// commandCatalogTier mirrors Claude-style discovery ownership: product
// commands first, then user and project custom commands, followed by MCP and
// other runtime sources. Sorting is alphabetical within each tier.
func commandCatalogTier(entry CommandCatalogEntry) int {
	switch strings.ToLower(strings.TrimSpace(entry.Source)) {
	case "", "repl", "slash":
		return 0
	case "user":
		return 1
	case "project":
		return 2
	default:
		return 3
	}
}

func commandCatalogGroup(entry CommandCatalogEntry) string {
	switch commandCatalogTier(entry) {
	case 0:
		return "Built-in commands"
	case 1:
		return "User commands"
	case 2:
		return "Project commands"
	default:
		return "MCP and other commands"
	}
}

// commandSourceBadge exposes non-built-in ownership without letting an MCP
// server-controlled name inject terminal controls. Keep only a compact set of
// identifier runes and cap the result for stable palette columns.
func commandSourceBadge(entry CommandCatalogEntry) string {
	if commandCatalogTier(entry) == 0 {
		return ""
	}
	raw := strings.TrimSpace(entry.Source)
	if raw == "" {
		raw = "other"
	}
	var b strings.Builder
	for _, r := range raw {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-_.:", r) {
			b.WriteRune(r)
		}
		if len([]rune(b.String())) >= 24 {
			break
		}
	}
	label := b.String()
	if label == "" {
		label = "other"
	}
	return "[" + label + "]"
}

// commandCatalog is the single user-visible command inventory.
func (m *Model) commandCatalog() []CommandCatalogEntry {
	return effectiveCommandCatalog(m.cmds, m.slash)
}

// preferSlashCommand records the remaining intentional Bubble Tea ownership
// decisions while handlers still live in two registries. Catalog construction
// follows this ownership because it describes the interactive TUI surface.
func preferSlashCommand(name string) bool {
	switch strings.ToLower(name) {
	case "memory", "doctor", "diff", "diff-view", "init":
		return true
	default:
		return false
	}
}

// Compatibility name retained for focused TUI routing tests.
func preferSlashInTUI(name string) bool { return preferSlashCommand(name) }

// preferSlashInPlainREPL is deliberately surface-specific. /memory has two
// long-standing implementations: the TUI slash command manages auto-memory
// files, while the line REPL command exposes read/write/search/clear. Sharing
// the TUI exception list silently changed scripted and accessible REPL usage.
func preferSlashInPlainREPL(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "doctor", "diff", "diff-view", "init":
		return true
	default:
		return false
	}
}

func (m *Model) customCommandCatalog() []CommandCatalogEntry {
	all := m.commandCatalog()
	out := make([]CommandCatalogEntry, 0, len(all))
	for _, cmd := range all {
		if cmd.Category == "custom" {
			out = append(out, cmd)
		}
	}
	return out
}
