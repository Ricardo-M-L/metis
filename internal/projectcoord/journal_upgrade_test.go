package projectcoord

import (
	"encoding/json"
	"os"
	"testing"
)

func TestLegacyJournalUpgradesBeforeAWriterCanDropNewLedgers(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(t.TempDir())
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Upgrade", Items: []WorkItemInput{{ID: "work", Subject: "Work", Prompt: "work"}}})
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.runPath(run.Workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	run.Version = legacyStateVersion
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	upgraded, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Version != stateVersion {
		t.Fatal("legacy journal was not upgraded atomically")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved Run
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Version != stateVersion {
		t.Fatal("old version-one writers would still accept the new journal")
	}
}
