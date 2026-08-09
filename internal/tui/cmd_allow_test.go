package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestCmdAllowPersistsAcrossGates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	gate := permission.New(permission.ModeDefault)
	out := cmdAllow(&REPL{Gate: gate}, "Bash")
	if !strings.Contains(out, "allowed permanently") {
		t.Fatalf("cmdAllow output=%q", out)
	}
	path := filepath.Join(home, "persistent-permissions.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("persistent permission file: %v", err)
	}

	reloaded := permission.New(permission.ModeDefault)
	if err := reloaded.LoadInto(permission.Default(home)); err != nil {
		t.Fatal(err)
	}
	decision, _ := reloaded.Check(t.Context(), "Bash", "echo ok")
	if decision != permission.DecisionAllow {
		t.Fatalf("reloaded decision=%v, want allow", decision)
	}
}
