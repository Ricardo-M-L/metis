package auth

import (
	"os"
	"testing"
)

// tempHomeSearch redirects METIS_HOME to a t.TempDir so each test
// gets a fresh ~/.metis/auth.json. Distinct name from auth_test.go's
// withTempHome helper because Go's package-level test files share
// one symbol table — two `withTempHome` declarations collide.
func tempHomeSearch(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	return dir
}

// TestSearchKey_RoundTrip — put → get returns what we wrote and the
// on-disk format places it under the "search:" namespace alongside
// (not colliding with) provider entries.
func TestSearchKey_RoundTrip(t *testing.T) {
	tempHomeSearch(t)
	if err := SetSearchKey("tavily", "tvly-test"); err != nil {
		t.Fatalf("SetSearchKey: %v", err)
	}
	got, err := GetSearchKey("tavily")
	if err != nil {
		t.Fatalf("GetSearchKey: %v", err)
	}
	if got != "tvly-test" {
		t.Errorf("GetSearchKey = %q, want tvly-test", got)
	}

	// Verify the on-disk shape: key stored under "search:tavily",
	// Type = "search" (so provider-iter code can tell them apart).
	f, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entry, ok := f["search:tavily"]
	if !ok {
		t.Fatalf("expected on-disk key %q to exist; got %+v", "search:tavily", f)
	}
	if entry.Type != "search" {
		t.Errorf("Entry.Type = %q, want \"search\"", entry.Type)
	}
}

// TestSearchKey_DoesNotAppearInProviderList — provider list (used by
// `metis auth list`) must not surface search-namespaced entries; a
// user listing providers shouldn't see search backends mixed in.
func TestSearchKey_DoesNotAppearInProviderList(t *testing.T) {
	tempHomeSearch(t)
	if err := Set("anthropic", "sk-prov"); err != nil {
		t.Fatalf("Set provider: %v", err)
	}
	if err := SetSearchKey("tavily", "tvly-x"); err != nil {
		t.Fatalf("SetSearchKey: %v", err)
	}
	providers, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, p := range providers {
		if p == "search:tavily" || p == "tavily" {
			t.Errorf("provider list leaked search key %q: %v", p, providers)
		}
	}
	searches, err := ListSearchKeys()
	if err != nil {
		t.Fatalf("ListSearchKeys: %v", err)
	}
	if len(searches) != 1 || searches[0] != "tavily" {
		t.Errorf("ListSearchKeys = %v, want [tavily]", searches)
	}
}

// TestSearchKey_GetMissing — querying an unset backend returns ""
// and no error so callers can fall through to env vars without
// special-casing.
func TestSearchKey_GetMissing(t *testing.T) {
	tempHomeSearch(t)
	got, err := GetSearchKey("brave")
	if err != nil {
		t.Errorf("GetSearchKey on missing should not error; got %v", err)
	}
	if got != "" {
		t.Errorf("GetSearchKey on missing = %q, want empty", got)
	}
}

// TestSearchKey_RemoveIsIdempotent — `metis auth keys rm brave`
// when brave was never set should silently succeed (matches the
// existing Remove() semantics for provider keys).
func TestSearchKey_RemoveIsIdempotent(t *testing.T) {
	tempHomeSearch(t)
	if err := RemoveSearchKey("brave"); err != nil {
		t.Errorf("RemoveSearchKey on missing should not error; got %v", err)
	}
}

// TestSearchKey_RemoveAfterPut — happy path: write, remove, verify
// the key is gone from both Get and ListSearchKeys.
func TestSearchKey_RemoveAfterPut(t *testing.T) {
	tempHomeSearch(t)
	_ = SetSearchKey("brave", "BSA-x")
	if err := RemoveSearchKey("brave"); err != nil {
		t.Fatalf("RemoveSearchKey: %v", err)
	}
	if v, _ := GetSearchKey("brave"); v != "" {
		t.Errorf("after remove, GetSearchKey = %q, want empty", v)
	}
	keys, _ := ListSearchKeys()
	if len(keys) != 0 {
		t.Errorf("after remove, ListSearchKeys = %v, want empty", keys)
	}
}

// TestSearchKey_ValidatesInputs — explicit empty backend/key both
// error out so a careless caller can't write a stub entry or
// silently no-op a fetch.
func TestSearchKey_ValidatesInputs(t *testing.T) {
	tempHomeSearch(t)
	if err := SetSearchKey("", "x"); err == nil {
		t.Error("SetSearchKey with empty backend should error")
	}
	if err := SetSearchKey("brave", ""); err == nil {
		t.Error("SetSearchKey with empty key should error")
	}
	if err := RemoveSearchKey(""); err == nil {
		t.Error("RemoveSearchKey with empty backend should error")
	}
}

// TestSearchKey_FilePermissionsAre0600 — auth.json on disk must
// stay chmod 0o600 after writing a search key. The whole point of
// preferring the persistent store over shell rc is the perm bump;
// regress on this and we silently lose the upgrade.
func TestSearchKey_FilePermissionsAre0600(t *testing.T) {
	tempHomeSearch(t)
	if err := SetSearchKey("tavily", "tvly-perm"); err != nil {
		t.Fatalf("SetSearchKey: %v", err)
	}
	st, err := os.Stat(Path())
	if err != nil {
		t.Fatalf("stat auth.json: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("auth.json perms = %#o, want 0o600", got)
	}
}
