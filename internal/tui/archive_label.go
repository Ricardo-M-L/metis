package tui

import (
	"strings"
	"unicode"

	"github.com/Ricardo-M-L/metis/internal/security"
)

// safeArchiveLabel is the only path by which a short, session-derived label
// should reach terminal output. Replace credential-shaped or control-bearing
// values wholesale and bound otherwise-valid labels so corrupt archives
// cannot inject terminal commands or flood a row.
func safeArchiveLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if security.Redact(raw) != raw || len(security.Scan(raw)) > 0 {
		return "[private]"
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return "[private]"
		}
	}
	runes := []rune(raw)
	const maxRunes = 64
	if len(runes) > maxRunes {
		raw = string(runes[:maxRunes-1]) + "…"
	}
	return raw
}
