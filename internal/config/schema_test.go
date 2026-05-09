package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerateConfigSchema_ProducesValidJSON(t *testing.T) {
	ResetSchemaCache()
	body, err := GenerateConfigSchema()
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatalf("schema must be valid JSON: %v", err)
	}
	if schema["$schema"] == nil {
		t.Error("expected top-level $schema field")
	}
}

func TestGenerateConfigSchema_TitleSet(t *testing.T) {
	ResetSchemaCache()
	body, _ := GenerateConfigSchema()
	if !strings.Contains(string(body), "metis configuration") {
		t.Error("title not set in schema")
	}
}

func TestGenerateConfigSchema_CoversTopLevelSections(t *testing.T) {
	// Every toml-tagged top-level Config field must appear in the
	// generated schema. Catches "added a Config field but forgot
	// to update something" — there's nothing to update, but if
	// invopop ever changes its tag-detection rules silently the
	// schema would suddenly miss fields.
	ResetSchemaCache()
	body, _ := GenerateConfigSchema()
	s := string(body)
	wanted := []string{"provider", "permission", "ui", "session", "tools", "loop_detection", "mcp", "channels", "hooks"}
	for _, w := range wanted {
		if !strings.Contains(s, `"`+w+`"`) {
			t.Errorf("schema missing top-level %q section: %s", w, firstFew(s))
		}
	}
}

func TestGenerateConfigSchema_UsesTOMLTagNames(t *testing.T) {
	// Confirm TOML tag names made it into the schema (vs JSON or
	// raw Go field names). E.g. `default_platform` is the toml tag
	// on Channels.DefaultPlatform — Go's JSON-default would emit
	// `defaultplatform` (lowercase no separator) which would NOT
	// match what the TOML file uses.
	ResetSchemaCache()
	body, _ := GenerateConfigSchema()
	if !strings.Contains(string(body), "default_platform") {
		t.Error("expected TOML tag default_platform in schema (snake_case from struct tag)")
	}
}

func TestGenerateConfigSchema_CachedOnSecondCall(t *testing.T) {
	ResetSchemaCache()
	a, err := GenerateConfigSchema()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateConfigSchema()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("repeat calls must return identical bytes (cache hit)")
	}
	// Defensive copy: mutating a must not affect the cache.
	if len(a) > 0 {
		a[0] = 'X'
	}
	c, _ := GenerateConfigSchema()
	if c[0] == 'X' {
		t.Error("cache returned a non-defensive-copy that callers can mutate")
	}
}

func TestResetSchemaCache_ClearsBetweenCalls(t *testing.T) {
	ResetSchemaCache()
	_, _ = GenerateConfigSchema()
	ResetSchemaCache()
	// After reset, second call regenerates — we can't observe the
	// regeneration directly, but at minimum the call must succeed
	// without panicking on a stale lock.
	if _, err := GenerateConfigSchema(); err != nil {
		t.Errorf("post-reset call should succeed: %v", err)
	}
}

func TestGenerateConfigSchema_AllowsAdditionalProperties(t *testing.T) {
	// We deliberately set AllowAdditionalProperties=true so a user
	// running an older metis with a newer schema (or vice versa)
	// doesn't get spurious "unknown property" warnings in their IDE.
	ResetSchemaCache()
	body, _ := GenerateConfigSchema()
	// Either explicit `"additionalProperties": true` OR absence
	// (default true) is acceptable. We assert no `false`.
	if strings.Contains(string(body), `"additionalProperties": false`) {
		t.Errorf("schema should not lock additionalProperties=false")
	}
}

func firstFew(s string) string {
	if len(s) > 200 {
		return s[:200] + "...[truncated]"
	}
	return s
}
