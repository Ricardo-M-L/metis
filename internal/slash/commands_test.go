package slash

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

func TestRegistryCatalogRepeatedRegistrationUsesEffectiveOwner(t *testing.T) {
	r := NewRegistry()
	r.Register(Cmd{Name: "check", Aliases: []string{"old"}, Description: "old"})
	r.Register(Cmd{Name: "check", Aliases: []string{"new"}, Description: "new"})

	all := r.Catalog()
	if len(all) != 1 || all[0].Description != "new" {
		t.Fatalf("effective catalog retained stale registration: %+v", all)
	}
	if _, ok := r.Resolve("old"); ok {
		t.Fatal("stale alias from replaced registration remained callable")
	}
	if len(all[0].Aliases) != 1 || all[0].Aliases[0] != "new" {
		t.Fatalf("catalog leaked stale aliases from replaced registration: %+v", all[0].Aliases)
	}
	if got, ok := r.CanonicalName("new"); !ok || got != "check" {
		t.Fatalf("CanonicalName(new) = %q, %v", got, ok)
	}
}

func TestRegistryCanonicalNameWinsAliasCollision(t *testing.T) {
	r := NewRegistry()
	r.Register(Cmd{Name: "first", Aliases: []string{"second"}})
	r.Register(Cmd{Name: "second"})

	if got, ok := r.CanonicalName("second"); !ok || got != "second" {
		t.Fatalf("canonical name was shadowed by alias: %q, %v", got, ok)
	}
	all := r.Catalog()
	if len(all) != 2 || len(all[0].Aliases) != 0 {
		t.Fatalf("catalog retained colliding alias: %+v", all)
	}
}

func TestRegistryCatalogConcurrentReadAndRegister(t *testing.T) {
	r := NewRegistry()
	r.Register(Cmd{Name: "base", Aliases: []string{"b"}})

	var wg sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = r.Catalog()
				_, _ = r.Resolve("base")
			}
		}()
	}
	for writer := 0; writer < 4; writer++ {
		writer := writer
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				r.Register(Cmd{Name: fmt.Sprintf("dynamic-%d-%d", writer, i)})
			}
		}()
	}
	wg.Wait()

	if got := len(r.Catalog()); got != 201 {
		t.Fatalf("catalog size = %d, want 201", got)
	}
}

func newRegistryWithBuiltins(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	cfg := &config.Config{}
	cfg.Provider.Default = "anthropic"
	RegisterAll(r, cfg)
	return r
}

func TestBuiltInCanonicalCommandMappings(t *testing.T) {
	r := newRegistryWithBuiltins(t)

	for _, name := range []string{"new", "clear", "reset"} {
		t.Run(name+" starts a new session", func(t *testing.T) {
			canonical, ok := r.CanonicalName(name)
			if !ok || canonical != "clear" {
				t.Fatalf("CanonicalName(%q) = %q, %v; want clear", name, canonical, ok)
			}
			handled, _, sig, _ := r.Parse("/" + name)
			if !handled || sig != SignalNew {
				t.Fatalf("/%s routed to handled=%v signal=%v, want SignalNew", name, handled, sig)
			}
		})
	}
	help := r.HelpText()
	hasClear := false
	for _, line := range strings.Split(help, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "/clear":
			hasClear = true
		case "/new", "/reset":
			t.Fatalf("help advertised alias as a separate command row: %q", line)
		}
	}
	if !hasClear {
		t.Fatalf("help must advertise only canonical /clear for the new-session command:\n%s", help)
	}

	for _, tc := range []struct {
		input     string
		canonical string
		signal    Signal
	}{
		{input: "clear-history", canonical: "clear-history", signal: SignalClear},
		{input: "update", canonical: "update", signal: SignalUpgrade},
		{input: "session-info", canonical: "session-info", signal: SignalSession},
		{input: "sid", canonical: "session-info", signal: SignalSession},
	} {
		t.Run(tc.input, func(t *testing.T) {
			canonical, ok := r.CanonicalName(tc.input)
			if !ok || canonical != tc.canonical {
				t.Fatalf("CanonicalName(%q) = %q, %v; want %q", tc.input, canonical, ok, tc.canonical)
			}
			handled, _, sig, _ := r.Parse("/" + tc.input)
			if !handled || sig != tc.signal {
				t.Fatalf("/%s routed to handled=%v signal=%v, want %v", tc.input, handled, sig, tc.signal)
			}
		})
	}

	for _, removed := range []string{"upgrade", "auto-memory"} {
		if _, ok := r.Resolve(removed); ok {
			t.Fatalf("removed command /%s is still callable", removed)
		}
	}
	if cmd, ok := r.Resolve("memory"); !ok || !strings.Contains(cmd.Description, "auto-memory") {
		t.Fatalf("original /memory auto-memory command changed: %+v", cmd)
	}
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

func TestParse_AcceptEditsShortcutEmitsModeSignal(t *testing.T) {
	r := newRegistryWithBuiltins(t)
	handled, display, sig, _ := r.Parse("/acceptEdits")
	if !handled {
		t.Fatal("/acceptEdits should be handled")
	}
	if sig != SignalAcceptEdits {
		t.Fatalf("/acceptEdits signal = %v, want SignalAcceptEdits", sig)
	}
	if display != "(mode: acceptEdits)" {
		t.Fatalf("/acceptEdits display = %q", display)
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
