package runtime

// test_helpers_test.go — shared helpers for runtime/ unit tests.
// setMetisHome was originally in internal/runtime/mcp_cache_test.go;
// when the MCP family was lifted into internal/runtime/mcp/ on
// 2026-05-20, the helper moved with cache_test.go but run_cache_test.go
// (still in runtime/) still references it. Keep a sibling copy here
// so neither test file has to reach across package boundaries.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// setMetisHome redirects config.Home() to a temp dir for the duration
// of one test. Verifies the redirection before returning so a
// regression in Home() shows up as a setup failure rather than
// mysterious cache misses downstream.
func setMetisHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	got := config.Home()
	if got != dir {
		t.Fatalf("config.Home() = %q, want %q (METIS_HOME redirection broken)", got, dir)
	}
	return dir
}
