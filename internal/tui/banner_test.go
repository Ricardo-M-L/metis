package tui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/version"
)

// minimalREPL constructs the smallest *REPL that printBanner needs:
// just an Out writer, a Gate (for mode), a model name, and the styles.
// Everything else is left zero — printBanner doesn't touch it.
func minimalREPL(out *bytes.Buffer, model string) *REPL {
	return &REPL{
		Gate:   permission.New(permission.ModeAuto),
		Styles: NewStyles(),
		model:  model,
		out:    out,
	}
}

// TestBanner_VersionFromPackage — the headline regression check.
// Banner must read version.Version, not the old hardcoded "0.1.0-dev".
// Mutating version.Version at test time and seeing it reflected proves
// the wiring; the package-level var is shared so we restore on cleanup.
func TestBanner_VersionFromPackage(t *testing.T) {
	saved := version.Version
	t.Cleanup(func() { version.Version = saved })

	// Test both bare numerics ("0.1.1") and pre-prefixed ("v0.1.1-...")
	// since `git describe` emits the latter while the bare VERSION file
	// has the former. Banner must render exactly one "v" prefix in both
	// cases (no "vv0.1.1" double-v regression).
	cases := []struct {
		input, wantSubstr string
	}{
		{"0.1.1", "v0.1.1"},
		{"9.9.9-rc1", "v9.9.9-rc1"},
		{"2.0.0", "v2.0.0"},
		{"dev-snapshot", "vdev-snapshot"},
		// Pre-prefixed inputs (git describe output) — must NOT double-v.
		{"v0.1.1-20-g0c8969d", "v0.1.1-20-g0c8969d"},
		{"v0.1.1", "v0.1.1"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			version.Version = tc.input
			var buf bytes.Buffer
			r := minimalREPL(&buf, "claude-opus-4-7")
			r.printBanner()
			out := stripANSI(buf.String())
			if !strings.Contains(out, tc.wantSubstr) {
				t.Errorf("banner missing %q; got:\n%s", tc.wantSubstr, out)
			}
			// Hard-block double-v regression.
			if strings.Contains(out, "vv") {
				t.Errorf("banner double-v regression: %q\n%s", tc.input, out)
			}
			// Anti-regression: the old hardcoded literal must never appear
			// (unless the active Version literally equals "0.1.0-dev").
			if tc.input != "0.1.0-dev" && strings.Contains(out, "v0.1.0-dev") {
				t.Errorf("banner still contains old hardcoded literal v0.1.0-dev:\n%s", out)
			}
		})
	}
}

// TestBanner_NeverHasOldLiteral — defense-in-depth: with the real
// production version.Version, the banner must not look like the old
// hardcoded value. Catches a future contributor copy/pasting the dev
// string back in.
func TestBanner_NeverHasOldLiteral(t *testing.T) {
	if version.Version == "0.1.0-dev" {
		t.Skip("version.Version legitimately equals the old literal — skipping anti-regression")
	}
	var buf bytes.Buffer
	r := minimalREPL(&buf, "claude-opus-4-7")
	r.printBanner()
	out := stripANSI(buf.String())
	if strings.Contains(out, "v0.1.0-dev") {
		t.Errorf("banner regressed to hardcoded v0.1.0-dev:\n%s", out)
	}
	if !strings.Contains(out, "v"+version.Version) {
		t.Errorf("banner missing v%s; got:\n%s", version.Version, out)
	}
}

// TestBanner_IncludesModelAndMode — the banner ships three pieces
// of info. Locking down all three here means a future change to
// printBanner can't accidentally drop one without a failing test.
func TestBanner_IncludesModelAndMode(t *testing.T) {
	var buf bytes.Buffer
	r := minimalREPL(&buf, "test-model-xyz")
	r.printBanner()
	out := stripANSI(buf.String())
	if !strings.Contains(out, "test-model-xyz") {
		t.Errorf("banner missing model name; got:\n%s", out)
	}
	if !strings.Contains(out, "auto") {
		t.Errorf("banner missing permission mode 'auto'; got:\n%s", out)
	}
	if !strings.Contains(out, "Metis") {
		t.Errorf("banner missing brand name; got:\n%s", out)
	}
}

// stripANSI drops ANSI color escape sequences so substring assertions
// match the rendered text rather than wrestling with lipgloss styles.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b { // ESC
			// skip until letter (CSI/SGR end byte) or end of string
			for i < len(s) && !((s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z')) {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
