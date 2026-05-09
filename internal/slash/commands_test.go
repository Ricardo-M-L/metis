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

// TestParse_PastedPathFallsThrough — bug audit 2026-05-10 (image #15).
// Pasted absolute paths (`/Users/...`, `/var/log/...`) start with `/`
// but are NOT slash commands. Parse must return handled=false so the
// caller can route them to the agent as plain prompt text. Without
// this, the user pasting a path sees a glaring "unknown: /Users/...
// — try /help" instead of the agent doing what they asked.
func TestParse_PastedPathFallsThrough(t *testing.T) {
	r := newRegistryWithBuiltins(t)
	cases := []string{
		"/Users/ricardo/Documents/foo",
		"/var/log/syslog",
		"/etc/passwd",
		"/tmp/x.txt",
		"/foo.bar baz",              // dotted name + args
		"/Users/x/path with spaces", // path + later text
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			handled, display, sig, _ := r.Parse(in)
			if handled {
				t.Errorf("path-like input %q should NOT be handled as slash; display=%q", in, display)
			}
			if display != "" {
				t.Errorf("path-like input should produce no display text; got %q", display)
			}
			if sig != SignalNone {
				t.Errorf("path-like input should produce SignalNone; got %v", sig)
			}
		})
	}
}

// TestParse_RealCommandsStillWork — regression guard so the path-detection
// doesn't break legitimate slash commands.
func TestParse_RealCommandsStillWork(t *testing.T) {
	r := newRegistryWithBuiltins(t)
	cases := []string{"/help", "/title my session", "/save", "/branch abc"}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			handled, _, _, _ := r.Parse(in)
			if !handled {
				t.Errorf("real command %q should be handled", in)
			}
		})
	}
}

func TestIsCommandShape(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"help", true},
		{"model", true},
		{"cmd-with-dash", true},
		{"cmd_with_underscore", true},
		{"abc123", true},

		{"", false},
		{"Users/ricardo", false}, // contains /
		{"foo.bar", false},       // contains .
		{"foo bar", false},       // contains space
		{"foo@host", false},      // contains @
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := IsCommandShape(tc.in); got != tc.want {
				t.Errorf("IsCommandShape(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
