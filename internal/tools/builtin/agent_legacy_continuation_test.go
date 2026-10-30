package builtin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestAgentLegacyDurableMigrationReportsAndRecordsUnknownSettings(t *testing.T) {
	dir, _, _, roster := childCheckpointFixture(t)
	provider := helloProvider()
	header := agent.NewSubAgentHeader("agt-legacy-mail", "m", "parent", "alice", dir, string(permission.ModeBypassPermissions))
	header.Provider = provider.Name()
	writer, err := agent.NewDurableSubAgentTranscript(dir, header.ID, header)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.BindHandoff("workspace-test", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Checkpoint([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "legacy work"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, agent.SubAgentTranscriptDirname, header.ID+".jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tool := NewAgent(permission.New(permission.ModeBypassPermissions), provider, tools.NewRegistry(), "m", "system").WithRoster(roster).WithSessionPersistence(dir, "parent")
	denied, err := tool.Execute(context.Background(), map[string]any{"resume_from": header.ID, "prompt": "continue", "allowed_tools": []any{"Read"}})
	if err != nil || denied == nil || !denied.IsError || !strings.Contains(denied.Output, "cannot change its profile or tool filters") {
		t.Fatalf("legacy filter override accepted: %+v %v", denied, err)
	}
	afterDenied, _ := os.ReadFile(path)
	if !bytes.Equal(before, afterDenied) {
		t.Fatal("refused migration rewrote acknowledged history")
	}
	result, err := tool.Execute(context.Background(), map[string]any{"resume_from": header.ID, "prompt": "continue"})
	if err != nil || result == nil || result.IsError || !strings.Contains(result.Output, "Legacy durable child migration") || result.Meta["legacy_migration"] != true {
		t.Fatalf("migration must disclose unknown settings: %+v %v", result, err)
	}
	snapshot, err := agent.LoadSubAgentSnapshot(dir, header.ID)
	if err != nil || snapshot.Execution == nil || !snapshot.Execution.LegacyMigration {
		t.Fatalf("migration was not durably distinguished: %+v %v", snapshot, err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.HasPrefix(after, before) {
		t.Fatal("migration replaced legacy history")
	}
}
