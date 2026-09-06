package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempHome points METIS_HOME at a fresh temp dir for the duration of t.
// auth.Path() reads METIS_HOME via config.Home(), so this scopes any reads /
// writes to the test sandbox without touching the user's real ~/.metis.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	return dir
}

func TestLoad_MissingFileReturnsEmptyMap(t *testing.T) {
	withTempHome(t)
	f, err := Load()
	if err != nil {
		t.Fatalf("Load on missing file should not error: %v", err)
	}
	if len(f) != 0 {
		t.Errorf("expected empty file, got %d entries", len(f))
	}
}

func TestSetAndGetRoundTrip(t *testing.T) {
	withTempHome(t)
	if err := Set("anthropic", "sk-test-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	k, err := Get("anthropic")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if k != "sk-test-123" {
		t.Errorf("got %q, want sk-test-123", k)
	}
}

func TestGetDoesNotReuseThirdPartyCredentialForBuiltInProvider(t *testing.T) {
	withTempHome(t)
	for provider, key := range map[string]string{
		"minimax": "sk-minimax-test",
		"gemini":  "sk-gemini-test",
		"groq":    "sk-groq-test",
	} {
		if err := Set(provider, key); err != nil {
			t.Fatalf("Set(%s): %v", provider, err)
		}
	}
	for _, provider := range []string{"anthropic", "openai"} {
		got, err := Get(provider)
		if err != nil {
			t.Fatalf("Get(%s): %v", provider, err)
		}
		if got != "" {
			t.Fatalf("Get(%s) reused a third-party credential", provider)
		}
	}
}

func TestSet_RejectsEmptyInputs(t *testing.T) {
	withTempHome(t)
	if err := Set("", "k"); err == nil {
		t.Error("Set with empty provider should error")
	}
	if err := Set("p", ""); err == nil {
		t.Error("Set with empty key should error")
	}
}

func TestSave_FilePermsAre0600(t *testing.T) {
	withTempHome(t)
	if err := Set("openai", "sk-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	st, err := os.Stat(Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mode := st.Mode().Perm()
	if mode != 0o600 {
		t.Errorf("auth.json perm = %o, want 600", mode)
	}
}

func TestRemove_DropsEntryAndReturnsNoErrorIfMissing(t *testing.T) {
	withTempHome(t)
	if err := Set("a", "x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := Set("b", "y"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := Remove("a"); err != nil {
		t.Fatalf("Remove existing: %v", err)
	}
	got, _ := Get("a")
	if got != "" {
		t.Errorf("after Remove, Get(a) = %q, want empty", got)
	}
	gotB, _ := Get("b")
	if gotB != "y" {
		t.Errorf("Remove(a) clobbered b = %q", gotB)
	}
	if err := Remove("c"); err != nil {
		t.Errorf("Remove of missing entry should not error, got %v", err)
	}
}

func TestList_SortedAndContainsSetEntries(t *testing.T) {
	withTempHome(t)
	for _, p := range []string{"openai", "anthropic", "minimax"} {
		if err := Set(p, "k"); err != nil {
			t.Fatalf("Set(%s): %v", p, err)
		}
	}
	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"anthropic", "minimax", "openai"}
	if len(got) != len(want) {
		t.Fatalf("List returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("List[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestSave_AtomicReplace(t *testing.T) {
	dir := withTempHome(t)
	if err := Set("anthropic", "sk-v1"); err != nil {
		t.Fatalf("Set v1: %v", err)
	}
	if err := Set("anthropic", "sk-v2"); err != nil {
		t.Fatalf("Set v2: %v", err)
	}
	// After the rewrite there must be exactly one auth.json (no .auth.json.*
	// temp leftovers from a botched cleanup).
	entries, err := os.ReadDir(filepath.Dir(Path()))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Base(e.Name()) == "auth.json" {
			count++
			continue
		}
		if strings.HasPrefix(e.Name(), ".auth.json.") {
			t.Errorf("unexpected auth tempfile leftover: %s", e.Name())
		}
	}
	if count != 1 {
		t.Errorf("auth.json count = %d, want 1", count)
	}
	if _, err := os.Stat(filepath.Join(dir, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy root-level auth.json unexpectedly remains: %v", err)
	}
	got, _ := Get("anthropic")
	if got != "sk-v2" {
		t.Errorf("after rewrite, key = %q, want sk-v2", got)
	}
}
