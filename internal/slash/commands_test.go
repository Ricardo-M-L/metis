package slash

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

func newRegistryWithBuiltins(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	cfg := &config.Config{}
	cfg.Provider.Default = "anthropic"
	RegisterAll(r, cfg)
	return r
}

func TestParse_ReturnsArgsForPayloadCommands(t *testing.T) {
	r := newRegistryWithBuiltins(t)
	handled, _, sig, args := r.Parse("/title refactor sprint plan")
	if !handled {
		t.Fatal("handled should be true for /title")
	}
	if sig != SignalTitle {
		t.Errorf("sig = %v, want SignalTitle", sig)
	}
	if args != "refactor sprint plan" {
		t.Errorf("args = %q, want 'refactor sprint plan'", args)
	}
}

func TestParse_BareTitleDoesNotEmitSignal(t *testing.T) {
	r := newRegistryWithBuiltins(t)
	handled, _, sig, _ := r.Parse("/title")
	if !handled {
		t.Fatal("handled should be true")
	}
	if sig != SignalNone {
		t.Errorf("/title alone should not emit SignalTitle; got %v", sig)
	}
}

func TestParse_BranchEmitsSignalBranch(t *testing.T) {
	r := newRegistryWithBuiltins(t)
	handled, _, sig, _ := r.Parse("/branch")
	if !handled {
		t.Fatal("handled should be true")
	}
	if sig != SignalBranch {
		t.Errorf("sig = %v, want SignalBranch", sig)
	}
}

func TestParse_SaveEmitsSignalSave(t *testing.T) {
	r := newRegistryWithBuiltins(t)
	handled, _, sig, _ := r.Parse("/save")
	if !handled {
		t.Fatal("handled should be true")
	}
	if sig != SignalSave {
		t.Errorf("sig = %v, want SignalSave", sig)
	}
}

func TestParse_NotASlashCommand(t *testing.T) {
	r := newRegistryWithBuiltins(t)
	handled, _, _, _ := r.Parse("just a message")
	if handled {
		t.Error("plain text should not be handled as a slash cmd")
	}
}

func TestParse_UnknownCommand(t *testing.T) {
	r := newRegistryWithBuiltins(t)
	handled, display, sig, _ := r.Parse("/zzzunknown")
	if !handled {
		t.Error("unknown slash should still be 'handled' (consumed)")
	}
	if sig != SignalNone {
		t.Errorf("unknown signal = %v, want SignalNone", sig)
	}
	if display == "" {
		t.Error("unknown command should produce a help hint")
	}
}
