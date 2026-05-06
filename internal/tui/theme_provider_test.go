package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestProviderAccentTint_Known(t *testing.T) {
	cases := map[string]string{
		"anthropic": "#d97757",
		"openai":    "#10a37f",
		"gemini":    "#4285f4",
		"kimi":      "#1e88e5",
		"moonshot":  "#1e88e5", // alias for the same vendor
	}
	for id, hexWant := range cases {
		want := lipgloss.Color(hexWant)
		got := ProviderAccentTint(id)
		if got != want {
			t.Errorf("ProviderAccentTint(%q) = %v; want %v", id, got, want)
		}
	}
}

func TestProviderAccentTint_CaseInsensitive(t *testing.T) {
	a := ProviderAccentTint("ANTHROPIC")
	b := ProviderAccentTint("anthropic")
	c := ProviderAccentTint("Anthropic")
	if a != b || b != c {
		t.Errorf("provider id should be case-insensitive; got %v / %v / %v", a, b, c)
	}
}

func TestProviderAccentTint_TrimsSpaces(t *testing.T) {
	a := ProviderAccentTint(" openai ")
	b := ProviderAccentTint("openai")
	if a != b {
		t.Errorf("leading/trailing whitespace should be trimmed; got %v vs %v", a, b)
	}
}

func TestProviderAccentTint_Unknown_FallsBack(t *testing.T) {
	got := ProviderAccentTint("not-a-real-provider")
	if got == nil {
		t.Error("unknown provider must return a fallback, not nil")
	}
}

func TestProviderAccentTint_Empty_FallsBack(t *testing.T) {
	got := ProviderAccentTint("")
	if got == nil {
		t.Error("empty provider must return a fallback, not nil")
	}
}

func TestThemeForProvider_KnownReturnsClone(t *testing.T) {
	base := currentTheme
	tinted := ThemeForProvider("openai")
	if tinted == base {
		t.Error("known provider should return a CLONE, not the shared currentTheme pointer")
	}
	if !strings.Contains(tinted.Name, "openai") {
		t.Errorf("clone name should reflect provider; got %q", tinted.Name)
	}
	if tinted.AccentBlue == base.AccentBlue {
		t.Error("AccentBlue should be retinted on a known provider")
	}
}

func TestThemeForProvider_UnknownReturnsSamePointer(t *testing.T) {
	got := ThemeForProvider("nonsense")
	if got != currentTheme {
		t.Error("unknown provider must NOT clone — return the active theme pointer for caller cheapness")
	}
}

func TestApplyProviderTint_NameDoesNotAccumulate(t *testing.T) {
	// Snapshot original to restore at end.
	originalName := currentTheme.Name
	t.Cleanup(func() {
		base := strings.SplitN(originalName, "+", 2)[0]
		SwitchTheme(base)
	})

	// Force a known starting point.
	SwitchTheme("dark")
	if currentTheme.Name != "dark" {
		t.Fatalf("setup: starting name should be 'dark'; got %q", currentTheme.Name)
	}

	ApplyProviderTint("anthropic")
	if currentTheme.Name != "dark+anthropic" {
		t.Errorf("first apply: expected dark+anthropic; got %q", currentTheme.Name)
	}

	// Crucial check: a second apply should NOT produce
	// "dark+anthropic+openai" — the prior +anthropic suffix must
	// be stripped before re-tinting.
	ApplyProviderTint("openai")
	if currentTheme.Name != "dark+openai" {
		t.Errorf("second apply: name should not accumulate, expected dark+openai; got %q",
			currentTheme.Name)
	}

	// Third apply should also stay clean.
	ApplyProviderTint("gemini")
	if currentTheme.Name != "dark+gemini" {
		t.Errorf("third apply: expected dark+gemini; got %q", currentTheme.Name)
	}
}

func TestThemeForProvider_DoesNotMutateGlobal(t *testing.T) {
	beforeName := currentTheme.Name
	beforeAccent := currentTheme.AccentBlue
	_ = ThemeForProvider("anthropic")
	if currentTheme.Name != beforeName {
		t.Errorf("global theme name mutated! before=%q after=%q", beforeName, currentTheme.Name)
	}
	if currentTheme.AccentBlue != beforeAccent {
		t.Error("global theme AccentBlue mutated!")
	}
}

func TestKnownProviderTints_SortedAndNonEmpty(t *testing.T) {
	got := KnownProviderTints()
	if len(got) == 0 {
		t.Fatal("at least the major providers should be listed")
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("KnownProviderTints not sorted: [%d]=%q > [%d]=%q",
				i-1, got[i-1], i, got[i])
		}
	}
	// Sanity: must include the headline providers.
	wantSeen := []string{"anthropic", "openai", "gemini"}
	for _, w := range wantSeen {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("KnownProviderTints missing major provider %q", w)
		}
	}
}
