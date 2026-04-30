package tui

import (
	"reflect"
	"sort"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/slash"
)

type stubSource struct{ items []string }

func (s *stubSource) Candidates() []string { return s.items }

func TestSlashCompleter_OnlyTriggersAfterSlash(t *testing.T) {
	c := &slashCompleter{source: &stubSource{items: []string{"help", "quit"}}}
	got, _ := c.Do([]rune("hel"), 3)
	if len(got) != 0 {
		t.Errorf("should not complete plain prose, got %v", got)
	}
}

func TestSlashCompleter_FiltersByPrefix(t *testing.T) {
	c := &slashCompleter{source: &stubSource{items: []string{"help", "hello", "quit"}}}
	got, length := c.Do([]rune("/he"), 3)
	if length != 2 {
		t.Errorf("expected length=2, got %d", length)
	}
	want := [][]rune{[]rune("lp"), []rune("llo")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", runesToStrings(got), runesToStrings(want))
	}
}

func TestSlashCompleter_NoMatch(t *testing.T) {
	c := &slashCompleter{source: &stubSource{items: []string{"help", "quit"}}}
	got, _ := c.Do([]rune("/zzz"), 4)
	if len(got) != 0 {
		t.Errorf("should be empty for no match, got %v", got)
	}
}

func TestSlashCompleter_EmptyPrefixListsAll(t *testing.T) {
	c := &slashCompleter{source: &stubSource{items: []string{"help", "quit"}}}
	got, _ := c.Do([]rune("/"), 1)
	if len(got) != 2 {
		t.Errorf("expected all 2 candidates, got %d", len(got))
	}
}

func TestSlashCompleter_NilSourceSafe(t *testing.T) {
	c := &slashCompleter{source: nil}
	got, _ := c.Do([]rune("/he"), 3)
	if got != nil {
		t.Errorf("nil source should yield nil candidates, got %v", got)
	}
}

func TestREPLCandidates_MergesAndDeduplicates(t *testing.T) {
	sl := slash.NewRegistry()
	sl.Register(slash.Cmd{Name: "help"})
	sl.Register(slash.Cmd{Name: "quit"})

	cmds := NewREPLCommandRegistry()
	cmds.Register(REPLCommand{Name: "help"}) // duplicate name
	cmds.Register(REPLCommand{Name: "tokens"})

	rc := &replCandidates{Slash: sl, REPL: cmds}
	got := rc.Candidates()
	sort.Strings(got)
	want := []string{"help", "quit", "tokens"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestREPLCandidates_NilSlashOK(t *testing.T) {
	cmds := NewREPLCommandRegistry()
	cmds.Register(REPLCommand{Name: "x"})
	rc := &replCandidates{Slash: nil, REPL: cmds}
	got := rc.Candidates()
	if len(got) != 1 || got[0] != "x" {
		t.Errorf("got %v, want [x]", got)
	}
}

func runesToStrings(rs [][]rune) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}
