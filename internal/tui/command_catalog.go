package tui

import (
	"sort"
	"strings"
)

// commandCatalog returns the commands users can actually type in the TUI.
// REPL commands win when both registries expose the same canonical name or
// alias because handleSubmit dispatches them first. Slash-only and custom
// commands are still included so the palette and /help describe the real
// command surface instead of only the first half of the routing table.
func (m *Model) commandCatalog() []REPLCommand {
	seen := make(map[string]bool)
	aliases := make(map[string]bool)
	out := make([]REPLCommand, 0, 96)
	if m.cmds != nil {
		for _, c := range m.cmds.All() {
			if preferSlashInTUI(c.Name) && m.slash != nil {
				continue
			}
			key := strings.ToLower(c.Name)
			if seen[key] {
				continue
			}
			seen[key] = true
			aliases[key] = true
			for _, alias := range c.Aliases {
				aliases[strings.ToLower(alias)] = true
			}
			out = append(out, c)
		}
	}
	if m.slash != nil {
		for _, c := range m.slash.All() {
			key := strings.ToLower(c.Name)
			if seen[key] || aliases[key] {
				continue
			}
			filteredAliases := make([]string, 0, len(c.Aliases))
			for _, alias := range c.Aliases {
				aliasKey := strings.ToLower(alias)
				if aliases[aliasKey] {
					continue
				}
				aliases[aliasKey] = true
				filteredAliases = append(filteredAliases, alias)
			}
			seen[key] = true
			aliases[key] = true
			out = append(out, REPLCommand{
				Name:        c.Name,
				Aliases:     filteredAliases,
				Description: c.Description,
			})
		}
	}
	return out
}

// preferSlashInTUI records the remaining intentional ownership decisions for
// names exposed by both legacy registries. These slash handlers have access to
// richer live runtime state than their old REPL counterparts. /agents remains
// REPL-owned because its handler is the canonical entry point for the live
// multi-agent tree screen.
func preferSlashInTUI(name string) bool {
	switch strings.ToLower(name) {
	case "memory", "doctor":
		return true
	default:
		return false
	}
}

func (m *Model) customCommandCatalog() []REPLCommand {
	if m.slash == nil {
		return nil
	}
	var out []REPLCommand
	for _, c := range m.slash.All() {
		if !c.Custom {
			continue
		}
		out = append(out, REPLCommand{
			Name:        c.Name,
			Aliases:     append([]string(nil), c.Aliases...),
			Description: c.Description,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
