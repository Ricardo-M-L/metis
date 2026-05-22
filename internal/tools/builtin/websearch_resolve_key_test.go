package builtin

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

// TestResolveSearchKey_EnvWinsOverAuthJSON — when both env and
// auth.json have a value, env must win. This matches the doc'd
// precedence (CI overrides should never silently lose to a stale
// persisted key) and mirrors how `Get()` already prefers env over
// stored provider credentials elsewhere.
func TestResolveSearchKey_EnvWinsOverAuthJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	if err := auth.SetSearchKey("tavily", "from-auth"); err != nil {
		t.Fatalf("seed auth.json: %v", err)
	}
	t.Setenv("TAVILY_API_KEY", "from-env")

	got := resolveSearchKey(webSearchBackend{name: "tavily", envVar: "TAVILY_API_KEY"})
	if got != "from-env" {
		t.Errorf("env should win over auth.json; got %q", got)
	}
}

// TestResolveSearchKey_FallsBackToAuthJSON — env unset, auth.json
// has the key: that's the persistent-store path. Empty env must
// trigger the fallback (Setenv("", "") to a non-existent var would
// not work; we instead clear the var explicitly).
func TestResolveSearchKey_FallsBackToAuthJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	t.Setenv("TAVILY_API_KEY", "")
	if err := auth.SetSearchKey("tavily", "from-auth-only"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got := resolveSearchKey(webSearchBackend{name: "tavily", envVar: "TAVILY_API_KEY"})
	if got != "from-auth-only" {
		t.Errorf("auth.json fallback failed; got %q", got)
	}
}

// TestResolveSearchKey_NoSourceReturnsEmpty — neither env nor
// auth.json: returns empty so the backend gets skipped silently.
func TestResolveSearchKey_NoSourceReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	t.Setenv("BRAVE_SEARCH_API_KEY", "")

	got := resolveSearchKey(webSearchBackend{name: "brave", envVar: "BRAVE_SEARCH_API_KEY"})
	if got != "" {
		t.Errorf("missing-everywhere should return empty; got %q", got)
	}
}

// TestResolveSearchKey_DDGNeedsNothing — backends with envVar=""
// (currently just DDG) must always return empty since they don't
// take a key — and must NOT consult auth.json.
func TestResolveSearchKey_DDGNeedsNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	// Even if someone weirdly set `search:ddg` in auth.json, the
	// DDG backend should ignore it — the http call doesn't accept
	// a key and would silently no-op or 401.
	_ = auth.SetSearchKey("ddg", "shouldnt-be-used")

	got := resolveSearchKey(webSearchBackend{name: "ddg", envVar: ""})
	if got != "" {
		t.Errorf("DDG backend should never resolve a key; got %q", got)
	}
}
