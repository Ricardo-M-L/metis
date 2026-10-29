package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestPlainREPLRewindExplainsWorkspacePolicyWithoutChangingHistory(t *testing.T) {
	// HOME now receives automatic sparse protection. The filesystem root
	// remains the policy-disabled workspace used by this error-path fixture.
	root := filepath.VolumeName(t.TempDir()) + string(os.PathSeparator)
	loop := &agent.Loop{Checkpointer: checkpoint.NewManager("plain-policy", root, t.TempDir()), Messages: []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "keep this prompt"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "keep this reply"}}},
	}}
	before := loop.History()
	r := &REPL{Loop: loop}
	output := r.rewindLastEditTurn()
	for _, expected := range []string{"disabled by workspace policy", "choose a project directory"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("plain rewind hid the workspace-policy reason %q: %s", expected, output)
		}
	}
	if !reflect.DeepEqual(loop.History(), before) {
		t.Fatal("policy-disabled code rewind changed conversation history")
	}
}
