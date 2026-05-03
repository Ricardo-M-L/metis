// Package tui owns the user-facing rendering: the prompt, streaming
// text, tool call display, permission dialogs. Implementation is line-
// oriented (println-based) so it composes with shell pipes and tmux.
//
// Why not bubbletea for v1: the chat UX is fundamentally append-only
// and works fine in a plain terminal. Bubbletea shines for full-screen
// apps with multiple focused widgets — overkill here, and it would
// fight piped/non-tty contexts.
package tui

import (
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
)

type Styles struct {
	UserPrompt    lipgloss.Style
	AssistantTag  lipgloss.Style
	ToolTag       lipgloss.Style
	ToolName      lipgloss.Style
	ToolArgs      lipgloss.Style
	ToolResult    lipgloss.Style
	ToolError     lipgloss.Style
	Info          lipgloss.Style
	Warn          lipgloss.Style
	Err           lipgloss.Style
	Tokens        lipgloss.Style
	Divider       lipgloss.Style
	PermissionBox lipgloss.Style
	Hint          lipgloss.Style
}

func NewStyles() *Styles {
	return &Styles{
		UserPrompt:    lipgloss.NewStyle().Foreground(lipgloss.Color("#5fafff")).Bold(true),
		AssistantTag:  lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true),
		ToolTag:       lipgloss.NewStyle().Foreground(lipgloss.Color("#ffb86c")),
		ToolName:      lipgloss.NewStyle().Foreground(lipgloss.Color("#ffb86c")).Bold(true),
		ToolArgs:      lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")),
		ToolResult:    lipgloss.NewStyle().Foreground(lipgloss.Color("#50fa7b")),
		ToolError:     lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5555")),
		Info:          lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd")).Italic(true),
		Warn:          lipgloss.NewStyle().Foreground(lipgloss.Color("#f1fa8c")),
		Err:           lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5555")).Bold(true),
		Tokens:        lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Italic(true),
		Divider:       lipgloss.NewStyle().Foreground(lipgloss.Color("#44475a")),
		PermissionBox: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Foreground(lipgloss.Color("#f1fa8c")).Padding(0, 1),
		Hint:          lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Italic(true),
	}
}

// MarkdownRenderer returns a glamour renderer with a sane width.
func MarkdownRenderer(width int) (*glamour.TermRenderer, error) {
	if width <= 0 {
		width = 80
	}
	if width > 120 {
		width = 120
	}
	return glamour.NewTermRenderer(
		glamour.WithEnvironmentConfig(),
		glamour.WithWordWrap(width),
	)
}
