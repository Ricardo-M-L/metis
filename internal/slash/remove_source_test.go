package slash

import "testing"

func TestRemoveSourceDropsOnlyMatchingCommands(t *testing.T) {
	r := NewRegistry()
	r.Register(Cmd{Name: "old_prompt", Aliases: []string{"old"}, Source: "mcp:server"})
	r.Register(Cmd{Name: "help", Source: "slash"})

	r.RemoveSource("mcp:server")

	if _, ok := r.Resolve("old_prompt"); ok {
		t.Fatal("canonical source command survived removal")
	}
	if _, ok := r.Resolve("old"); ok {
		t.Fatal("alias from removed source survived removal")
	}
	if _, ok := r.Resolve("help"); !ok {
		t.Fatal("unrelated source command was removed")
	}
}
