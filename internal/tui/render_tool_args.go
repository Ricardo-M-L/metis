package tui

import (
	"strings"
	"unicode"

	xansi "github.com/charmbracelet/x/ansi"
)

// Tool inputs remain untouched; only their terminal presentation is sanitized.
func toolArgsDisplayText(value string) string {
	value = strings.ReplaceAll(xansi.Strip(value), "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\u0085', ' ', ' ':
			return '\n'
		case '\t':
			return ' '
		}
		if unicode.IsControl(r) && r != '\n' {
			return -1
		}
		return r
	}, value)
}

func toolArgsFirstLine(value string) string {
	value = strings.TrimSpace(toolArgsDisplayText(value))
	line, _, multiline := strings.Cut(value, "\n")
	if multiline {
		return strings.TrimRight(line, " ") + " …"
	}
	return line
}

// Expanded transcript mode reveals the command hidden by the compact leader,
// preserving its line structure and indenting every physical continuation row.
func renderExpandedToolCommand(command, preview, indent string, width int) string {
	command = toolArgsDisplayText(strings.ReplaceAll(command, "\t", "    "))
	if command == "" || command == preview {
		return ""
	}
	if width > 0 {
		command = xansi.Hardwrap(command, max(1, width-xansi.StringWidth(indent)), true)
	}
	return styleDim.Render(indent+"command:") + "\n" +
		indent + strings.ReplaceAll(command, "\n", "\n"+indent) + "\n"
}
