package i18n

import "testing"

// TestParity_AllLocalesCoverEnglishKeys — opencode-style parity:
// every locale must have a translation for every key the canonical
// (en) locale defines. Catches drift when a contributor adds a new
// English string but forgets the zh-CN side.
func TestParity_AllLocalesCoverEnglishKeys(t *testing.T) {
	for tag, dict := range locales {
		if tag == "en" {
			continue
		}
		for k := range en {
			if _, ok := dict[k]; !ok {
				t.Errorf("locale %q missing key %q", tag, k)
			}
		}
	}
}

// TestT_FallsBackToEnglish — when the active locale doesn't have a
// key, T returns the English value rather than the bare key.
func TestT_FallsBackToEnglish(t *testing.T) {
	SetLocale("zh-CN")
	defer SetLocale("")
	// Inject a key only in en to exercise fallback.
	en["__test_fallback_key__"] = "english value"
	defer delete(en, "__test_fallback_key__")
	if got := T("__test_fallback_key__"); got != "english value" {
		t.Errorf("expected fallback to en; got %q", got)
	}
}

// TestT_UsesActiveLocale — happy path: active locale has the key.
func TestT_UsesActiveLocale(t *testing.T) {
	SetLocale("zh-CN")
	defer SetLocale("")
	got := T("perm.allow")
	if got == "Yes" {
		t.Errorf("zh-CN should localise perm.allow; got english")
	}
	if got != "是" {
		t.Errorf("expected zh-CN value '是'; got %q", got)
	}
}

// TestT_UnknownKeyReturnsKey — last-resort fallback for missing
// translations on both sides.
func TestT_UnknownKeyReturnsKey(t *testing.T) {
	if got := T("never.registered.key"); got != "never.registered.key" {
		t.Errorf("expected raw key fallback; got %q", got)
	}
}
