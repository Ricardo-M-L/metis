package tui

import (
	"encoding/json"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// jsonPrettyOrEmpty marshals v to indented JSON; returns "" on error
// or empty input so the caller can omit the section.
func jsonPrettyOrEmpty(v map[string]any) string {
	if len(v) == 0 {
		return ""
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// skillDetailScreen builds the DetailScreen for a picked skill name.
// Returns nil when the skill isn't loadable (caller silently no-ops).
func (m *Model) skillDetailScreen(name string) *screen.DetailScreen {
	list, err := loadSkillCatalog(m.loop, m.skillDir)
	if err != nil {
		return nil
	}
	for _, sk := range list {
		if sk.Name != name {
			continue
		}
		sections := []screen.DetailSection{}
		if sk.Description != "" {
			sections = append(sections, screen.DetailSection{
				Heading: "Description",
				Lines:   wrapLines(sk.Description, 100),
			})
		}
		if sk.WhenToUse != "" {
			sections = append(sections, screen.DetailSection{
				Heading: "When to use",
				Lines:   wrapLines(sk.WhenToUse, 100),
			})
		}
		if len(sk.AllowedTools) > 0 {
			sections = append(sections, screen.DetailSection{
				Heading: "Allowed tools",
				Lines:   []string{strings.Join(sk.AllowedTools, " · ")},
			})
		}
		if len(sk.Tags) > 0 {
			sections = append(sections, screen.DetailSection{
				Heading: "Tags",
				Lines:   []string{strings.Join(sk.Tags, " · ")},
			})
		}
		if sk.Version != "" {
			sections = append(sections, screen.DetailSection{
				Heading: "Version",
				Lines:   []string{sk.Version},
			})
		}
		if sk.Prompt != "" {
			sections = append(sections, screen.DetailSection{
				Heading: "Prompt body",
				Lines:   strings.Split(sk.Prompt, "\n"),
			})
		}
		return screen.NewDetailScreen("/skills", name, sections).WithParent("skills")
	}
	return nil
}

// toolDetailScreen builds the DetailScreen for a picked tool name.
func (m *Model) toolDetailScreen(name string) *screen.DetailScreen {
	if m.loop == nil || m.loop.Registry == nil {
		return nil
	}
	if entry, ok := m.loop.Registry.GetModelEntry(name); ok {
		t := entry.Tool
		sections := []screen.DetailSection{
			{
				Heading: "Description",
				Lines:   wrapLines(t.Description(), 100),
			},
		}
		// InputSchema returns the input shape — pretty-print as text so
		// the user sees what arguments the tool accepts.
		if schemaText := jsonPrettyOrEmpty(t.InputSchema()); schemaText != "" {
			sections = append(sections, screen.DetailSection{
				Heading: "Input schema (JSON)",
				Lines:   strings.Split(schemaText, "\n"),
			})
		}
		sections = append(sections, screen.DetailSection{
			Heading: "Note",
			Lines: []string{
				"Tools are dispatched by the agent, not invoked directly by the user.",
				"This screen is a reference — you can't run the tool from here.",
			},
		})
		return screen.NewDetailScreen("/tools", name, sections).WithParent("tools")
	}
	return nil
}

// wrapLines is a deliberately tiny soft-wrap so DetailScreen rows
// don't run off the right edge of the modal.
func wrapLines(s string, width int) []string {
	if width <= 20 || len(s) <= width {
		return []string{s}
	}
	var out []string
	for len(s) > width {
		cut := width
		// Prefer a space within the last 25 chars so we don't split
		// mid-word.
		for i := width - 1; i >= width-25 && i >= 0; i-- {
			if s[i] == ' ' {
				cut = i
				break
			}
		}
		out = append(out, s[:cut])
		s = strings.TrimLeft(s[cut:], " ")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
