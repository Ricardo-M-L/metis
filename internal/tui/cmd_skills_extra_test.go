package tui

// Unit tests for the Phase-A /skills subcommands. Each test gets its
// own skillDir under t.TempDir(); we never reach for ~/.metis/skills.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
)

// newSkillTestREPL gives us the bare REPL state the /skills helpers
// need (skillDir only). Avoids constructing the full agent loop +
// session store + permission gate just to flip a Disabled flag.
func newSkillTestREPL(t *testing.T) (*REPL, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "skills")
	r := &REPL{skillDir: dir}
	return r, dir
}

func TestSkills_Create_ThenInfo(t *testing.T) {
	r, dir := newSkillTestREPL(t)

	out := r.handleSkillCreate("debug-helper")
	if !strings.Contains(out, "created skill") {
		t.Fatalf("create should confirm; got: %q", out)
	}

	// File on disk?
	store := skills.NewStore(dir)
	sk, err := store.Get("debug-helper")
	if err != nil {
		t.Fatalf("get after create: %v", err)
	}
	if sk.Description == "" {
		t.Errorf("scaffold should leave a description placeholder")
	}

	out = r.handleSkillInfo("debug-helper")
	if !strings.Contains(out, "debug-helper") || !strings.Contains(out, "enabled") {
		t.Errorf("info should render name + state; got:\n%s", out)
	}
}

func TestSkills_Create_RefusesDuplicate(t *testing.T) {
	r, _ := newSkillTestREPL(t)
	r.handleSkillCreate("dup")
	out := r.handleSkillCreate("dup")
	if !strings.Contains(out, "already exists") {
		t.Errorf("dup create should refuse; got: %q", out)
	}
}

func TestSkills_EnableDisable_RoundTrip(t *testing.T) {
	r, dir := newSkillTestREPL(t)
	r.handleSkillCreate("toggle-me")

	out := r.handleSkillDisable("toggle-me")
	if !strings.Contains(out, "disabled skill") {
		t.Fatalf("disable should confirm; got: %q", out)
	}
	store := skills.NewStore(dir)
	sk, _ := store.Get("toggle-me")
	if !sk.Disabled {
		t.Fatalf("expected sk.Disabled=true after disable")
	}

	// Idempotent.
	out = r.handleSkillDisable("toggle-me")
	if !strings.Contains(out, "already disabled") {
		t.Errorf("re-disable should be idempotent; got: %q", out)
	}

	out = r.handleSkillEnable("toggle-me")
	if !strings.Contains(out, "enabled skill") {
		t.Fatalf("enable should confirm; got: %q", out)
	}
	sk, _ = store.Get("toggle-me")
	if sk.Disabled {
		t.Fatalf("expected sk.Disabled=false after enable")
	}
}

func TestSkills_EnableUnknown(t *testing.T) {
	r, _ := newSkillTestREPL(t)
	out := r.handleSkillEnable("ghost-skill")
	if !strings.Contains(out, "no skill named") {
		t.Errorf("expected 'no skill named'; got: %q", out)
	}
}

func TestSkills_SearchLocal(t *testing.T) {
	r, _ := newSkillTestREPL(t)
	r.handleSkillCreate("alpha-helper")
	r.handleSkillCreate("beta-helper")
	r.handleSkillCreate("gamma-thing")

	out := r.handleSkillSearchLocal("helper")
	// Expect two hits — alpha and beta — but not gamma.
	if !strings.Contains(out, "alpha-helper") {
		t.Errorf("expected alpha-helper match; got:\n%s", out)
	}
	if !strings.Contains(out, "beta-helper") {
		t.Errorf("expected beta-helper match; got:\n%s", out)
	}
	if strings.Contains(out, "gamma-thing") {
		t.Errorf("gamma-thing should NOT match 'helper' query; got:\n%s", out)
	}
}

func TestSkills_SearchLocal_NoHits(t *testing.T) {
	r, _ := newSkillTestREPL(t)
	r.handleSkillCreate("only-one")
	out := r.handleSkillSearchLocal("nonexistent-needle")
	if !strings.Contains(out, "no installed skills match") {
		t.Errorf("expected no-hit hint; got: %q", out)
	}
}

func TestSkills_Dispatcher_UnknownSubcommand(t *testing.T) {
	r, _ := newSkillTestREPL(t)
	out := cmdSkills(r, "frobnicate foo")
	if !strings.Contains(out, "unknown") {
		t.Errorf("expected unknown subcommand error; got: %q", out)
	}
}

func TestSkills_Dispatcher_InfoRoute(t *testing.T) {
	r, _ := newSkillTestREPL(t)
	r.handleSkillCreate("router-test")
	out := cmdSkills(r, "info router-test")
	if !strings.Contains(out, "router-test") {
		t.Errorf("expected dispatcher to route to info handler; got:\n%s", out)
	}
}
