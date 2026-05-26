package mcp

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

	in := ServerEntry{
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
	in := ServerEntry{
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
	reg := &Registry{}
	cases := []string{
		"/usr/bin/something-else",
		"metis-cu",       // even the canonical binary must go through /cu
		"./bin/metis-cu", // path variants don't bypass the guard
		"npx",
	}
	for _, command := range cases {
		err := AddServer(reg, "computer-use", command, nil)
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

// TestAddMCPServerWithEnv_StoresAndCopies — env supplied to the with-env
// variant is preserved on the entry and the original map is detached
// (mutating the caller's map can't bleed into the stored entry).
func TestAddMCPServerWithEnv_StoresAndCopies(t *testing.T) {
	reg := &Registry{}
	src := map[string]string{"FIRECRAWL_API_KEY": "fc-abc", "OTHER": "v"}
	if err := AddServerWithEnv(reg, "fc", "node", []string{"server.js"}, src); err != nil {
		t.Fatalf("AddServerWithEnv: %v", err)
	}
	got := FindServer(reg, "fc")
	if got == nil {
		t.Fatalf("entry not inserted")
	}
	if got.Env["FIRECRAWL_API_KEY"] != "fc-abc" {
		t.Errorf("env not preserved; got %v", got.Env)
	}
	// Mutate caller map → stored entry must not see it.
	src["FIRECRAWL_API_KEY"] = "tampered"
	if got.Env["FIRECRAWL_API_KEY"] != "fc-abc" {
		t.Errorf("AddServerWithEnv should copy env; got %v", got.Env)
	}
}

// TestAddMCPServer_NilEnvLeavesFieldNil — plain AddServer (no env)
// shouldn't allocate an empty Env map; TOML serialization should skip
// the key entirely thanks to `omitempty`.
func TestAddMCPServer_NilEnvLeavesFieldNil(t *testing.T) {
	reg := &Registry{}
	if err := AddServer(reg, "plain", "cmd", nil); err != nil {
		t.Fatal(err)
	}
	got := FindServer(reg, "plain")
	if got == nil {
		t.Fatalf("entry missing")
	}
	if got.Env != nil {
		t.Errorf("Env should be nil for no-env add; got %v", got.Env)
	}
}

// TestExpandEnvVarsInEntry_EnvFieldExpands — values in the Env field
// get the same ${VAR} treatment as command/args/headers.
func TestExpandEnvVarsInEntry_EnvFieldExpands(t *testing.T) {
	t.Setenv("METIS_TEST_FCKEY", "fc-123")
	entry := ServerEntry{
		Name:    "fc",
		Command: "node",
		Env:     map[string]string{"FIRECRAWL_API_KEY": "${METIS_TEST_FCKEY}"},
	}
	expanded, missing := expandEnvVarsInEntry(entry)
	if expanded.Env["FIRECRAWL_API_KEY"] != "fc-123" {
		t.Errorf("env value not expanded; got %v", expanded.Env)
	}
	if len(missing) != 0 {
		t.Errorf("nothing should be missing; got %v", missing)
	}
}

// TestExpandEnvVarsInEntry_EnvFieldMissingReported — a missing var in
// Env surfaces in the missing slice so launch can refuse with a clear
// error, same as missing vars in headers/args.
func TestExpandEnvVarsInEntry_EnvFieldMissingReported(t *testing.T) {
	entry := ServerEntry{
		Name:    "fc",
		Command: "node",
		Env:     map[string]string{"FIRECRAWL_API_KEY": "${METIS_NEVER_SET_FCKEY}"},
	}
	_, missing := expandEnvVarsInEntry(entry)
	if len(missing) != 1 || missing[0] != "METIS_NEVER_SET_FCKEY" {
		t.Errorf("missing var should be reported; got %v", missing)
	}
}

// TestEnvSliceFromMap_StableOrder — the slice for exec.Cmd.Env must be
// sorted so test assertions and stderr-capture in diagnostics don't
// flap across runs.
func TestEnvSliceFromMap_StableOrder(t *testing.T) {
	out := envSliceFromMap(map[string]string{"B": "2", "A": "1", "C": "3"})
	want := []string{"A=1", "B=2", "C=3"}
	if len(out) != len(want) {
		t.Fatalf("len=%d want %d", len(out), len(want))
	}
	for i, w := range want {
		if out[i] != w {
			t.Errorf("[%d]=%q want %q", i, out[i], w)
		}
	}
}

// TestEnvSliceFromMap_EmptyNil — nil/empty input returns nil so the
// caller's "if len(extraEnv) > 0" guard skips cleanly.
func TestEnvSliceFromMap_EmptyNil(t *testing.T) {
	if out := envSliceFromMap(nil); out != nil {
		t.Errorf("nil input should yield nil; got %v", out)
	}
	if out := envSliceFromMap(map[string]string{}); out != nil {
		t.Errorf("empty input should yield nil; got %v", out)
	}
}

// TestSetReservedComputerUseServer_RoundTrip — /cu enable's path. First
// call inserts (replaced=false); second call returns replaced=true and
// the entry's command stays pinned to the canonical binary regardless
// of what was there before.
func TestSetReservedComputerUseServer_RoundTrip(t *testing.T) {
	reg := &Registry{}
	if replaced := SetReservedComputerUseServer(reg); replaced {
		t.Errorf("first call should report replaced=false on empty registry")
	}
	if got := FindServer(reg, ReservedComputerUseName); got == nil {
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

// TestMaybeInjectCUEnv_AddsTierForMetisCu — 2026-05-26: every metis-cu
// MCP spawn should auto-set METIS_CU_HOST_TERMINAL_TIER=full so the
// frontmost-app tier gate (terminal app = TierClick) doesn't reject
// `open_application` and friends. Covers basename detection across
// bare command, absolute path, and Windows .exe.
func TestMaybeInjectCUEnv_AddsTierForMetisCu(t *testing.T) {
	for _, cmd := range []string{
		"metis-cu",
		"/Users/ricardo/.local/bin/metis-cu",
		"C:\\bin\\metis-cu.exe",
		"Metis-Cu.exe", // case-insensitive
	} {
		out := maybeInjectCUEnv(cmd, nil)
		if got := out["METIS_CU_HOST_TERMINAL_TIER"]; got != "full" {
			t.Errorf("cmd=%q: tier env = %q, want full", cmd, got)
		}
	}
}

// TestMaybeInjectCUEnv_LeavesOthersAlone — only metis-cu spawns
// should pick up the override. Defensive: a regression here would
// silently inject the env into unrelated MCP servers (firecrawl,
// playwright, office-word, ...) and could break their config.
func TestMaybeInjectCUEnv_LeavesOthersAlone(t *testing.T) {
	for _, cmd := range []string{
		"npx",
		"uvx",
		"python3",
		"/usr/local/bin/firecrawl-mcp",
		"playwright-mcp",
	} {
		out := maybeInjectCUEnv(cmd, map[string]string{"FOO": "bar"})
		if _, set := out["METIS_CU_HOST_TERMINAL_TIER"]; set {
			t.Errorf("cmd=%q: must NOT inject tier env into non-metis-cu spawns", cmd)
		}
		// Existing entries must round-trip untouched.
		if out["FOO"] != "bar" {
			t.Errorf("cmd=%q: existing env dropped; got %v", cmd, out)
		}
	}
}

// TestMaybeInjectCUEnv_RespectsUserOverride — the user setting
// METIS_CU_HOST_TERMINAL_TIER explicitly in their [env] block must
// win over our default. The user might want "read" for a paranoid
// sandboxed run, or "click" to opt back into the historical default.
func TestMaybeInjectCUEnv_RespectsUserOverride(t *testing.T) {
	in := map[string]string{"METIS_CU_HOST_TERMINAL_TIER": "click"}
	out := maybeInjectCUEnv("metis-cu", in)
	if got := out["METIS_CU_HOST_TERMINAL_TIER"]; got != "click" {
		t.Errorf("user-set tier should be preserved; got %q want click", got)
	}
}

// TestMaybeInjectCUEnv_NilInputSafe — defensive: a nil env map is
// the common case (most users don't set [env] at all) and must not
// panic when we add the tier key.
func TestMaybeInjectCUEnv_NilInputSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil env caused panic: %v", r)
		}
	}()
	out := maybeInjectCUEnv("metis-cu", nil)
	if out["METIS_CU_HOST_TERMINAL_TIER"] != "full" {
		t.Errorf("nil env case lost the tier injection")
	}
}
