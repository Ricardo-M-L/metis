package builtin

// lsp_isenabled_test.go pins LSP's self-aware availability check —
// the real demo case for the IsEnabled() interface contract added
// 2026-05-14. LSP shells out to `gopls`, so it sets IsEnabled() to
// the result of exec.LookPath("gopls"). When gopls is absent the
// tool is hidden from the model entirely, not just exposed as a tool
// that always errors.

import (
	"os/exec"
	"testing"
)

func TestLSP_IsEnabled_TracksGoplsAvailability(t *testing.T) {
	// Whatever the host has, LSP.IsEnabled must agree with the
	// authoritative exec.LookPath result. We can't reliably toggle
	// PATH in a test (other tests run in parallel and modifying the
	// env races), so we just assert the invariant: LSP's answer
	// matches LookPath's answer right now.
	_, lookupErr := exec.LookPath("gopls")
	want := lookupErr == nil

	got := LSP{}.IsEnabled()
	if got != want {
		t.Errorf("LSP.IsEnabled() = %v; exec.LookPath(\"gopls\") said %v — these must agree", got, want)
	}
}
