package runtime

import (
	"strings"
	"testing"
)

// TestExpandEnvVarsInString_PlainSubstitution — happy path: ${VAR}
// resolves against the environment, ${MISSING} stays as the literal
// AND the missing name is reported.
func TestExpandEnvVarsInString_PlainSubstitution(t *testing.T) {
	t.Setenv("METIS_TEST_TOKEN", "abc123")
	got, missing := expandEnvVarsInString("Bearer ${METIS_TEST_TOKEN}")
	if got != "Bearer abc123" {
		t.Errorf("expected 'Bearer abc123'; got %q", got)
	}
	if len(missing) != 0 {
		t.Errorf("expected no missing; got %v", missing)
	}
}

// TestExpandEnvVarsInString_DefaultValue — ${VAR:-fallback} returns
// fallback when VAR is unset, and resolves to VAR when set.
func TestExpandEnvVarsInString_DefaultValue(t *testing.T) {
	got, missing := expandEnvVarsInString("${METIS_NEVER_SET:-default-val}")
	if got != "default-val" {
		t.Errorf("expected 'default-val'; got %q", got)
	}
	if len(missing) != 0 {
		t.Errorf("default fallback shouldn't add to missing; got %v", missing)
	}

	t.Setenv("METIS_NOW_SET", "real")
	got, _ = expandEnvVarsInString("${METIS_NOW_SET:-default-val}")
	if got != "real" {
		t.Errorf("env value should win over default; got %q", got)
	}
}

// TestExpandEnvVarsInString_MissingReported — bare ${VAR} with no
// default and no env value leaves the literal in place AND surfaces
// the missing name. Silent empty-substitution (the wrong fix) would
// produce a "Bearer " header that fails authentication opaquely.
func TestExpandEnvVarsInString_MissingReported(t *testing.T) {
	got, missing := expandEnvVarsInString("Bearer ${METIS_NOT_DEFINED_XYZ}")
	if !strings.Contains(got, "${METIS_NOT_DEFINED_XYZ}") {
		t.Errorf("missing var should stay as literal; got %q", got)
	}
	if len(missing) != 1 || missing[0] != "METIS_NOT_DEFINED_XYZ" {
		t.Errorf("expected missing=[METIS_NOT_DEFINED_XYZ]; got %v", missing)
	}
}

// TestExpandEnvVarsInString_MissingDeduped — multiple occurrences of
// the same missing var collapse to one entry in `missing`.
func TestExpandEnvVarsInString_MissingDeduped(t *testing.T) {
	_, missing := expandEnvVarsInString("${X_FOO} and ${X_FOO} and ${X_FOO}")
	if len(missing) != 1 {
		t.Errorf("dups should collapse; got %v", missing)
	}
}

// TestExpandEnvVarsInEntry_AllFields — verifies expansion runs across
// command, args, url, and headers.
func TestExpandEnvVarsInEntry_AllFields(t *testing.T) {
	t.Setenv("METIS_BIN_PATH", "/usr/local/bin/some-mcp")
	t.Setenv("METIS_ARG", "--verbose")
	t.Setenv("METIS_HOST", "https://api.example.com")
	t.Setenv("METIS_TOKEN", "secret")

	in := MCPServerEntry{
		Name:    "test",
		Command: "${METIS_BIN_PATH}",
		Args:    []string{"${METIS_ARG}", "--port", "8080"},
		URL:     "${METIS_HOST}/mcp",
		Headers: map[string]string{
			"Authorization": "Bearer ${METIS_TOKEN}",
			"X-Static":      "no-vars-here",
		},
	}
	out, missing := expandEnvVarsInEntry(in)
	if len(missing) != 0 {
		t.Errorf("all set, expected no missing; got %v", missing)
	}
	if out.Command != "/usr/local/bin/some-mcp" {
		t.Errorf("command not expanded; got %q", out.Command)
	}
	if out.Args[0] != "--verbose" || out.Args[1] != "--port" {
		t.Errorf("args not expanded properly; got %v", out.Args)
	}
	if out.URL != "https://api.example.com/mcp" {
		t.Errorf("url not expanded; got %q", out.URL)
	}
	if out.Headers["Authorization"] != "Bearer secret" {
		t.Errorf("header not expanded; got %q", out.Headers["Authorization"])
	}
	if out.Headers["X-Static"] != "no-vars-here" {
		t.Errorf("static header should pass through; got %q", out.Headers["X-Static"])
	}
}

// TestExpandEnvVarsInEntry_MissingDeduped — a single missing var
// referenced from multiple fields shows up exactly once.
func TestExpandEnvVarsInEntry_MissingDeduped(t *testing.T) {
	in := MCPServerEntry{
		Command: "${METIS_GHOST}",
		Args:    []string{"${METIS_GHOST}"},
		Headers: map[string]string{"X": "${METIS_GHOST}"},
	}
	_, missing := expandEnvVarsInEntry(in)
	if len(missing) != 1 || missing[0] != "METIS_GHOST" {
		t.Errorf("expected one deduped missing; got %v", missing)
	}
}

// TestAddMCPServer_ReservedNameRefused — every `/mcp add computer-use ...`
// path must fail, regardless of which binary the user names. The reserved
// slot is owned by metis's built-in (set via /cu enable / the dedicated
// SetReservedComputerUseServer API). This matches Claude Code's
// addMcpConfig behavior; Codex doesn't reserve names but it also has no
// built-in computer-use server to protect.
func TestAddMCPServer_ReservedNameRefused(t *testing.T) {
	reg := &MCPRegistry{}
	cases := []string{
		"/usr/bin/something-else",
		"metis-cu",       // even the canonical binary must go through /cu
		"./bin/metis-cu", // path variants don't bypass the guard
		"npx",
	}
	for _, command := range cases {
		err := AddMCPServer(reg, "computer-use", command, nil)
		if err == nil {
			t.Errorf("`/mcp add computer-use %s` should be refused", command)
			continue
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("error for command %q should mention 'reserved'; got %q",
				command, err.Error())
		}
	}
	// Sanity: the registry stays empty after all those refusals.
	if len(reg.Servers) != 0 {
		t.Errorf("refused adds shouldn't mutate registry; got %d entries", len(reg.Servers))
	}
}

// TestSetReservedComputerUseServer_RoundTrip — /cu enable's path. First
// call inserts (replaced=false); second call returns replaced=true and
// the entry's command stays pinned to the canonical binary regardless
// of what was there before.
func TestSetReservedComputerUseServer_RoundTrip(t *testing.T) {
	reg := &MCPRegistry{}
	if replaced := SetReservedComputerUseServer(reg); replaced {
		t.Errorf("first call should report replaced=false on empty registry")
	}
	if got := FindMCPServer(reg, ReservedComputerUseName); got == nil {
		t.Fatalf("entry not inserted")
	} else if got.Command != ReservedComputerUseBinary {
		t.Errorf("command should be pinned to %q; got %q",
			ReservedComputerUseBinary, got.Command)
	}
	// Second call: same name, replaced=true.
	if replaced := SetReservedComputerUseServer(reg); !replaced {
		t.Errorf("second call should report replaced=true")
	}
	if len(reg.Servers) != 1 {
		t.Errorf("set should not duplicate; got %d entries", len(reg.Servers))
	}
}
