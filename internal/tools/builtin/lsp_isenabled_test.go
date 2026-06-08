package builtin

// lsp_isenabled_test.go pins LSP's self-aware availability check — the
// IsEnabled() interface contract (2026-05-14). LSP is now multi-backend:
// it's enabled when ANY supported server (gopls / pyright /
// typescript-language-server / rust-analyzer) is on PATH, and hidden from
// the model only when none is present.

import (
	"os/exec"
	"testing"
)

func TestLSP_IsEnabled_TracksAnyBackend(t *testing.T) {
	// IsEnabled must agree with the authoritative "any backend on PATH"
	// answer. We can't reliably toggle PATH (parallel tests race on env),
	// so we assert the invariant against the live environment.
	want := anyLSPServerAvailable()
	if got := (LSP{}).IsEnabled(); got != want {
		t.Errorf("LSP.IsEnabled() = %v; anyLSPServerAvailable() = %v — must agree", got, want)
	}
}

func TestAnyLSPServerAvailable_GoplsImpliesEnabled(t *testing.T) {
	// If gopls is present, the tool must be enabled regardless of the
	// other backends.
	if _, err := exec.LookPath("gopls"); err == nil {
		if !anyLSPServerAvailable() {
			t.Error("gopls is on PATH but anyLSPServerAvailable() = false")
		}
	}
}
