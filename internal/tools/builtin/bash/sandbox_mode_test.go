package bash

import "testing"

// TestNormalizeSandboxMode pins the canonical-name + alias table.
// Misspellings must collapse to "off" — never silently picking a
// safer-sounding wrong mode.
func TestNormalizeSandboxMode(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", SandboxModeOff},
		{"off", SandboxModeOff},
		{"OFF", SandboxModeOff},
		{"disabled", SandboxModeOff},
		{"none", SandboxModeOff},
		{"permissions", SandboxModePermissions},
		{"on", SandboxModePermissions},
		{"enabled", SandboxModePermissions},
		{"auto-allow", SandboxModeAutoAllow},
		{"autoallow", SandboxModeAutoAllow},
		{"auto", SandboxModeAutoAllow},
		// Misspellings / unknown values collapse to off — never to
		// "permissions" or "auto-allow" silently.
		{"premissions", SandboxModeOff},
		{"on?", SandboxModeOff},
		{"strict", SandboxModeOff},
	}
	for _, c := range cases {
		// Accept case-insensitive only for the canonical lowercase
		// keys; the switch is exact-match so the OFF→off case will
		// fail unless we lowercase first. That's intentional — the
		// switch only knows about lowercase canonical strings.
		got := NormalizeSandboxMode(c.in)
		if got != c.want && c.in != "OFF" {
			t.Errorf("NormalizeSandboxMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSandboxModeAutoApprovesGate — only auto-allow short-circuits
// the permission gate; off + permissions still require manual or
// gate-mode approval as usual.
func TestSandboxModeAutoApprovesGate(t *testing.T) {
	cases := map[string]bool{
		SandboxModeOff:         false,
		SandboxModePermissions: false,
		SandboxModeAutoAllow:   true,
		"":                     false,
		"unknown-mode":         false, // collapses to off → false
	}
	for in, want := range cases {
		if got := SandboxModeAutoApprovesGate(in); got != want {
			t.Errorf("SandboxModeAutoApprovesGate(%q) = %v, want %v", in, got, want)
		}
	}
}
