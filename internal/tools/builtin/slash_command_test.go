package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

type fakeSlashRunner struct {
	out  string
	err  error
	last string
}

func (f *fakeSlashRunner) RunForModel(command string) (string, error) {
	f.last = command
	return f.out, f.err
}
func (f *fakeSlashRunner) Names() []string { return []string{"standup", "review"} }

func TestSlashCommand_RunsAndReturnsOutput(t *testing.T) {
	r := &fakeSlashRunner{out: "expanded recipe body"}
	tool := NewSlashCommand(permission.New(permission.ModeBypass), r)
	res, err := tool.Execute(context.Background(), map[string]any{"command": "/standup api"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("unexpected error result: %q", res.Output)
	}
	if res.Output != "expanded recipe body" {
		t.Errorf("output = %q", res.Output)
	}
	if r.last != "/standup api" {
		t.Errorf("runner got %q, want \"/standup api\"", r.last)
	}
}

func TestSlashCommand_RequiresCommand(t *testing.T) {
	tool := NewSlashCommand(permission.New(permission.ModeBypass), &fakeSlashRunner{})
	res, _ := tool.Execute(context.Background(), map[string]any{"command": "  "})
	if !res.IsError {
		t.Error("empty command should be an error result")
	}
}

func TestSlashCommand_NilRunnerDisabled(t *testing.T) {
	tool := NewSlashCommand(permission.New(permission.ModeBypass), nil)
	res, _ := tool.Execute(context.Background(), map[string]any{"command": "/x"})
	if !res.IsError || !strings.Contains(res.Output, "unavailable") {
		t.Errorf("nil runner should disable gracefully; got %q", res.Output)
	}
}

func TestSlashCommand_DescriptionListsAvailable(t *testing.T) {
	tool := NewSlashCommand(permission.New(permission.ModeBypass), &fakeSlashRunner{})
	d := tool.Description()
	if !strings.Contains(d, "standup") || !strings.Contains(d, "review") {
		t.Errorf("description should list available commands; got %q", d)
	}
}
