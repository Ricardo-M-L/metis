package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestUsageUnknownProviderRendersAlignedWithoutDividerColon(t *testing.T) {
	r := &REPL{providerName: "sensenova", model: "sensenova-6.8-flash-lite"}
	content := cmdUsage(r, "")
	if strings.Contains(stripANSI(content), "session totals ──:") {
		t.Fatalf("section divider rendered as a key with a colon:\n%s", stripANSI(content))
	}
	role := classifyREPLOutput(content)
	if role != "info" {
		t.Fatalf("role = %q, want info", role)
	}
	rendered := stripANSI(renderMessage(Message{Role: role, Content: content}, 120, false))
	lines := strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "    ╭") {
		t.Fatalf("unexpected top border: %q", rendered)
	}
	if strings.ContainsAny(lines[0], "·⚠✓✗") {
		t.Fatalf("bordered usage panel carried a status glyph: %q", lines[0])
	}
	wantWidth := lipgloss.Width(lines[0])
	for i, line := range lines {
		if !strings.HasPrefix(line, "    ") {
			t.Fatalf("usage line %d is misaligned: %q", i, line)
		}
		border := strings.IndexAny(line, "╭│╰")
		if border < 0 {
			t.Fatalf("usage line %d has no left border: %q", i, line)
		}
		prefix := line[:border]
		if got := lipgloss.Width(prefix); got != 4 {
			t.Fatalf("usage line %d border column = %d, want 4: %q", i, got, line)
		}
		if got := visibleWidthEA(prefix); got != 4 {
			t.Fatalf("usage line %d East-Asian border column = %d, want 4: %q", i, got, line)
		}
		if gotWidth := lipgloss.Width(line); gotWidth != wantWidth {
			t.Fatalf("usage row %d width = %d, want %d: %q", i, gotWidth, wantWidth, line)
		}
	}

	plain := stripANSI(content)
	if !strings.Contains(plain, "provider:       sensenova") ||
		!strings.Contains(plain, "model:          sensenova-6.8-flash-lite") {
		t.Fatalf("usage key/value columns are not aligned:\n%s", plain)
	}
	for _, key := range []string{"provider", "model", "dashboard", "input", "output", "cache_create", "cache_read", "cache hit rate"} {
		marker := key + ":"
		var row string
		for _, line := range lines {
			if strings.Contains(line, "│ "+marker) {
				row = line
				break
			}
		}
		if row == "" {
			t.Fatalf("missing usage row %q in:\n%s", key, rendered)
		}
		valueStart := strings.Index(row, marker) + len(marker)
		for valueStart < len(row) && row[valueStart] == ' ' {
			valueStart++
		}
		if got := lipgloss.Width(row[:valueStart]); got != 22 {
			t.Fatalf("value column for %q = %d, want 22: %q", key, got, row)
		}
	}
	for _, row := range lines {
		if divider := strings.Index(row, "── session totals ──"); divider >= 0 {
			if got := lipgloss.Width(row[:divider]); got != 6 {
				t.Fatalf("divider column = %d, want 6: %q", got, row)
			}
			return
		}
	}
	t.Fatal("session totals divider missing")
}
