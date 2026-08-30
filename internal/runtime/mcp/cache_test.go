package mcp

// mcp_cache_test.go — locks the on-disk schema cache + lazy-launch
// behavior added in P7. Coverage focuses on the cache key (fingerprint
// stability + invalidation) and the load/save round-trip; the lazy
// server's spawn-on-first-call semantics are exercised in
// mcp_lazy_test.go where we can mock the spawn closure cleanly.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	mcpsdk "github.com/Ricardo-M-L/metis/internal/mcp"
)

// setMetisHome redirects config.Home() to a temp dir for the duration
// of one test. Returns a cleanup that restores the prior value.
// Matches the pattern other runtime tests use; pulled into a helper
// here so test bodies stay readable.
func setMetisHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	// config.Home() reads $METIS_HOME — confirm the redirection works
	// before we hand the dir out, so a regression in Home() shows up
	// as a setup failure rather than mysterious cache misses.
	got := config.Home()
	if got != dir {
		t.Fatalf("config.Home() = %q, want %q (METIS_HOME redirection broken)", got, dir)
	}
	return dir
}

// TestFingerprintEntry_StableForIdenticalInput — the same inputs MUST
// produce the same fingerprint across calls, otherwise the cache
// flaps on every restart. Map iteration order is the usual source of
// drift; we sort keys explicitly in FingerprintEntry to defuse it.
func TestFingerprintEntry_StableForIdenticalInput(t *testing.T) {
	e := ServerEntry{
		Name:    "test",
		Command: "/usr/bin/python",
		Args:    []string{"-m", "mcp_server"},
		Headers: map[string]string{"X-Token": "abc", "X-User": "metis"},
	}
	a := FingerprintEntry(e)
	for i := 0; i < 50; i++ {
		if got := FingerprintEntry(e); got != a {
			t.Fatalf("fingerprint flapped on call %d: %q vs first %q", i, got, a)
		}
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Errorf("fingerprint should be sha256: prefixed; got %q", a)
	}
}

// TestFingerprintEntry_ChangesOnAnyField — every input axis the
// fingerprint covers (command / args / url / headers) MUST change the
// result. If a field is silently ignored, edits to mcp.toml wouldn't
// invalidate the cache and the user would silently keep using stale
// schemas.
func TestFingerprintEntry_ChangesOnAnyField(t *testing.T) {
	base := ServerEntry{
		Command: "a", Args: []string{"x"}, URL: "u",
		Headers: map[string]string{"h": "1"}, Env: map[string]string{"E": "1"},
		Auth: "static", EnabledTools: []string{"one"}, DisabledTools: []string{"two"},
	}
	baseFP := FingerprintEntry(base)
	mutants := []struct {
		label string
		mut   func(*ServerEntry)
	}{
		{"command", func(e *ServerEntry) { e.Command = "b" }},
		{"args", func(e *ServerEntry) { e.Args = []string{"y"} }},
		{"args-additional", func(e *ServerEntry) { e.Args = []string{"x", "extra"} }},
		{"url", func(e *ServerEntry) { e.URL = "v" }},
		{"working directory", func(e *ServerEntry) { e.WorkingDir = "plugin-root" }},
		{"auth", func(e *ServerEntry) { e.Auth = "oauth" }},
		{"env-value", func(e *ServerEntry) { e.Env = map[string]string{"E": "2"} }},
		{"env-key", func(e *ServerEntry) { e.Env = map[string]string{"F": "1"} }},
		{"enabled-tools", func(e *ServerEntry) { e.EnabledTools = []string{"other"} }},
		{"disabled-tools", func(e *ServerEntry) { e.DisabledTools = []string{"other"} }},
		{"disabled", func(e *ServerEntry) { e.Disabled = true }},
		{"header-value", func(e *ServerEntry) { e.Headers = map[string]string{"h": "2"} }},
		{"header-key", func(e *ServerEntry) { e.Headers = map[string]string{"j": "1"} }},
		{"header-extra", func(e *ServerEntry) { e.Headers = map[string]string{"h": "1", "k": "2"} }},
	}
	for _, m := range mutants {
		t.Run(m.label, func(t *testing.T) {
			cpy := base
			m.mut(&cpy)
			if got := FingerprintEntry(cpy); got == baseFP {
				t.Errorf("%s mutation did not change fingerprint", m.label)
			}
		})
	}
}

// TestFingerprintEntry_NameNotIncluded — a rename in mcp.toml shouldn't
// invalidate the cache if the launch identity (command/args/url/headers)
// is unchanged. The file name on disk handles the rename dimension.
func TestFingerprintEntry_NameNotIncluded(t *testing.T) {
	a := ServerEntry{Name: "old", Command: "c"}
	b := ServerEntry{Name: "new", Command: "c"}
	if FingerprintEntry(a) != FingerprintEntry(b) {
		t.Errorf("rename shouldn't change fingerprint")
	}
}

func TestFingerprintEntry_MapAndFilterOrderDoesNotMatter(t *testing.T) {
	a := ServerEntry{
		Command:      "server",
		Headers:      map[string]string{"A": "1", "B": "2"},
		Env:          map[string]string{"C": "3", "D": "4"},
		EnabledTools: []string{"one", "two"}, DisabledTools: []string{"three", "four"},
	}
	b := ServerEntry{
		Command:      "server",
		Headers:      map[string]string{"B": "2", "A": "1"},
		Env:          map[string]string{"D": "4", "C": "3"},
		EnabledTools: []string{"two", "one"}, DisabledTools: []string{"four", "three"},
	}
	if got, want := FingerprintEntry(a), FingerprintEntry(b); got != want {
		t.Fatalf("equivalent map/set ordering changed fingerprint:\n%s\n%s", got, want)
	}
}

// TestLoadMCPCache_MissingFileReturnsNil — the "no cache yet" case
// (every server on first run). Callers treat (nil, nil) as the
// signal to spawn-and-cache; an error would force them to handle
// "is this just a missing file vs a real read error" everywhere.
func TestLoadMCPCache_MissingFileReturnsNil(t *testing.T) {
	setMetisHome(t)
	got, err := LoadCache("never-existed")
	if err != nil {
		t.Errorf("missing cache should return nil, nil; got err=%v", err)
	}
	if got != nil {
		t.Errorf("missing cache should return nil cache; got %+v", got)
	}
}

// TestLoadMCPCache_MalformedJSONErrors — corrupted cache must surface
// as an error so the eager-spawn path is taken instead of silently
// downgrading to "no cache".
func TestLoadMCPCache_MalformedJSONErrors(t *testing.T) {
	dir := setMetisHome(t)
	cacheDir := filepath.Join(dir, "mcp-cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "broken.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCache("broken")
	if err == nil {
		t.Errorf("malformed cache should error; got nil")
	}
}

func TestLoadMCPCache_RejectsLegacyUnredactedVersion(t *testing.T) {
	dir := setMetisHome(t)
	cacheDir := filepath.Join(dir, "mcp-cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"version":2,"fingerprint":"sha256:legacy","tools":[{"name":"x","description":"possibly-secret"}]}`
	if err := os.WriteFile(CachePath("legacy"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCache("legacy")
	if err != nil {
		t.Fatalf("load legacy cache: %v", err)
	}
	if got != nil {
		t.Fatalf("legacy cache without current redaction version was accepted: %+v", got)
	}
}

// TestSaveLoadMCPCache_RoundTrip — write + read returns the same
// data. Tests both the JSON shape stays stable and the file
// permissions / location are correct.
func TestSaveLoadMCPCache_RoundTrip(t *testing.T) {
	setMetisHome(t)
	want := &Cache{
		Fingerprint: "sha256:test",
		Tools: []CachedTool{
			{Name: "alpha", Description: "first", InputSchema: map[string]any{"type": "object"}},
			{Name: "beta", Description: "second", InputSchema: map[string]any{"type": "object", "x": 1.0}},
		},
	}
	if err := SaveCache("svr", want); err != nil {
		t.Fatalf("save: %v", err)
	}
	if want.CachedAt == "" {
		t.Errorf("SaveCache should populate CachedAt; got empty")
	}
	if want.Version != cacheFormatVersion {
		t.Errorf("SaveCache version = %d, want %d", want.Version, cacheFormatVersion)
	}
	got, err := LoadCache("svr")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil {
		t.Fatalf("load returned nil after save")
	}
	if got.Fingerprint != want.Fingerprint {
		t.Errorf("fingerprint round-trip: got %q want %q", got.Fingerprint, want.Fingerprint)
	}
	if len(got.Tools) != len(want.Tools) {
		t.Fatalf("tools count: got %d want %d", len(got.Tools), len(want.Tools))
	}
	for i, w := range want.Tools {
		if got.Tools[i].Name != w.Name {
			t.Errorf("tool[%d].Name: got %q want %q", i, got.Tools[i].Name, w.Name)
		}
		// JSON round-trip turns numbers into float64; compare via
		// marshal/unmarshal to dodge int/float drift.
		wj, _ := json.Marshal(w.InputSchema)
		gj, _ := json.Marshal(got.Tools[i].InputSchema)
		if string(wj) != string(gj) {
			t.Errorf("tool[%d].InputSchema: got %s want %s", i, gj, wj)
		}
	}
}

// TestSaveMCPCache_FilePerms — cache files contain command lines and
// tool params that may reveal sensitive paths. Match Save's
// stance: 0o600.
func TestSaveMCPCache_FilePerms(t *testing.T) {
	setMetisHome(t)
	if err := SaveCache("perm", &Cache{Fingerprint: "x"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(CachePath("perm"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("cache perms: got %#o want 0o600", mode)
	}
}

// TestCachedToolsToMCPTools_RoundTrip — the converters are how the
// cache shape and the live mcp.Tool shape stay in sync. A regression
// here would manifest as "model can't see schemas it just fetched"
// or "cache fingerprint mismatch on every save" — easier to catch
// here than at integration time.
func TestCachedToolsToMCPTools_RoundTrip(t *testing.T) {
	original := []mcpsdk.Tool{
		{Name: "a", Description: "d1", InputSchema: map[string]any{"t": "object"}},
		{Name: "b", Description: "d2", InputSchema: map[string]any{"t": "object", "n": 2.0}},
	}
	cached := MCPToolsToCached(original)
	back := CachedToolsToMCPTools(cached)
	if len(back) != len(original) {
		t.Fatalf("len: got %d want %d", len(back), len(original))
	}
	for i := range original {
		if back[i].Name != original[i].Name {
			t.Errorf("Name[%d]", i)
		}
		oj, _ := json.Marshal(original[i].InputSchema)
		bj, _ := json.Marshal(back[i].InputSchema)
		if string(oj) != string(bj) {
			t.Errorf("InputSchema[%d]: got %s want %s", i, bj, oj)
		}
	}
}

// TestParseLazyMCPMode — the env-var matrix. Locks the documented
// aliases ("true"/"yes"/"1") so a refactor of ParseLazyMode can't
// silently change which strings switch to "always".
func TestParseLazyMCPMode(t *testing.T) {
	cases := []struct {
		in   string
		want LazyMode
	}{
		{"", LazyMCPModeAuto},
		{"auto", LazyMCPModeAuto},
		{"AUTO", LazyMCPModeAuto},
		{" auto ", LazyMCPModeAuto},
		{"unknown", LazyMCPModeAuto},
		{"always", LazyMCPModeAlways},
		{"ALWAYS", LazyMCPModeAlways},
		{"true", LazyMCPModeAlways},
		{"yes", LazyMCPModeAlways},
		{"1", LazyMCPModeAlways},
		{"never", LazyMCPModeNever},
		{"false", LazyMCPModeNever},
		{"no", LazyMCPModeNever},
		{"0", LazyMCPModeNever},
		{"off", LazyMCPModeNever},
	}
	for _, c := range cases {
		if got := ParseLazyMode(c.in); got != c.want {
			t.Errorf("ParseLazyMode(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func writeCacheFixture(t *testing.T, serverName string, cache *Cache) {
	t.Helper()
	if err := os.MkdirAll(CacheDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CachePath(serverName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCacheAPIsRejectUnsafeServerNames(t *testing.T) {
	home := setMetisHome(t)
	outside := filepath.Join(home, "escape.json")
	legacyTraversalTarget := &Cache{
		Version: cacheFormatVersion,
		Tools:   []CachedTool{{Name: "must-not-load"}},
	}
	raw, err := json.Marshal(legacyTraversalTarget)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"../escape", `..\\escape`, "contains space", "line\nbreak", "中文", ".", ".."} {
		t.Run(name, func(t *testing.T) {
			if got, err := LoadCache(name); err == nil || got != nil {
				t.Fatalf("LoadCache(%q) = (%+v, %v), want rejection", name, got, err)
			}
			if err := SaveCache(name, &Cache{}); err == nil {
				t.Fatalf("SaveCache(%q) unexpectedly succeeded", name)
			}
		})
	}
}

func TestCachePathFailsClosedForUnsafeServerName(t *testing.T) {
	setMetisHome(t)
	if got := CachePath("../escape"); got != "" {
		t.Fatalf("CachePath returned traversable path %q for unsafe server name", got)
	}
}

func TestLoadCacheRejectsOversizedFile(t *testing.T) {
	setMetisHome(t)
	if err := os.MkdirAll(CacheDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	oversized := bytes.Repeat([]byte("x"), maxCacheFileBytes+1)
	if err := os.WriteFile(CachePath("oversized"), oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCache("oversized"); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("oversized cache error = %v, want explicit size-limit rejection", err)
	}
}

func TestCacheRejectsTooManyToolsOnLoadAndSave(t *testing.T) {
	setMetisHome(t)
	cache := &Cache{Version: cacheFormatVersion, Tools: make([]CachedTool, maxCachedTools+1)}
	for i := range cache.Tools {
		cache.Tools[i] = CachedTool{Name: "tool", InputSchema: map[string]any{"type": "object"}}
	}
	writeCacheFixture(t, "many-tools", cache)
	if _, err := LoadCache("many-tools"); err == nil {
		t.Fatal("cache with too many tools unexpectedly loaded")
	}
	if err := SaveCache("many-tools-save", cache); err == nil {
		t.Fatal("cache with too many tools unexpectedly saved")
	}
}

func TestCacheRejectsOversizedDescriptionOnLoadAndSave(t *testing.T) {
	setMetisHome(t)
	cache := &Cache{Version: cacheFormatVersion, Tools: []CachedTool{{
		Name:        "tool",
		Description: strings.Repeat("d", maxCachedToolDescriptionBytes+1),
		InputSchema: map[string]any{"type": "object"},
	}}}
	writeCacheFixture(t, "large-description", cache)
	if _, err := LoadCache("large-description"); err == nil {
		t.Fatal("cache with oversized description unexpectedly loaded")
	}
	if err := SaveCache("large-description-save", cache); err == nil {
		t.Fatal("cache with oversized description unexpectedly saved")
	}
}

func TestCacheRejectsOversizedSchemaOnLoadAndSave(t *testing.T) {
	setMetisHome(t)
	cache := &Cache{Version: cacheFormatVersion, Tools: []CachedTool{{
		Name: "tool",
		InputSchema: map[string]any{
			"type": "object",
			"blob": strings.Repeat("s", maxCachedToolSchemaBytes+1),
		},
	}}}
	writeCacheFixture(t, "large-schema", cache)
	if _, err := LoadCache("large-schema"); err == nil {
		t.Fatal("cache with oversized schema unexpectedly loaded")
	}
	if err := SaveCache("large-schema-save", cache); err == nil {
		t.Fatal("cache with oversized schema unexpectedly saved")
	}
}

func TestCacheRejectsOversizedDecodedPayloadOnLoadAndSave(t *testing.T) {
	setMetisHome(t)
	cache := &Cache{Version: cacheFormatVersion, Fingerprint: "sha256:test"}
	for i := 0; i < (maxCacheDecodedBytes/maxCachedToolSchemaBytes)+1; i++ {
		cache.Tools = append(cache.Tools, CachedTool{
			Name: "tool",
			InputSchema: map[string]any{
				"type": "object",
				"blob": strings.Repeat("t", maxCachedToolSchemaBytes-256),
			},
		})
	}
	writeCacheFixture(t, "large-total", cache)
	if _, err := LoadCache("large-total"); err == nil {
		t.Fatal("cache with oversized decoded payload unexpectedly loaded")
	}
	if err := SaveCache("large-total-save", cache); err == nil {
		t.Fatal("cache with oversized decoded payload unexpectedly saved")
	}
}
