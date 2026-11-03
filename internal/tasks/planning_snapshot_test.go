package tasks

import (
	"bytes"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestPlanningItemsSnapshotUsesCanonicalMergeWithoutWriting(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "planning-snapshot-merge"
	if err := Save(&List{SessionID: sid, Items: []Item{{
		ID: "todo", Content: "1. Build changed code", Status: "pending", UpdatedAt: time.Now().Add(-time.Hour),
	}}}); err != nil {
		t.Fatal(err)
	}
	writer := NewTaskStore(sid)
	created, err := writer.Create("Build changed code", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	status, owner := TaskInProgress, "builder"
	if _, err := writer.Update(created.ID, TaskPatch{Status: &status, Owner: &owner}); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, file := range []string{path(sid), taskFilePath(sid)} {
		before[file], err = os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
	}
	canonical, err := PlanningItems(sid)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := PlanningItemsSnapshot(sid)
	if err != nil || !reflect.DeepEqual(snapshot, canonical) {
		t.Fatalf("snapshot/canonical merge differ: snapshot=%+v canonical=%+v err=%v", snapshot, canonical, err)
	}
	if len(snapshot) != 1 || snapshot[0].ID != "todo" || snapshot[0].Status != "in_progress" || snapshot[0].Owner != owner {
		t.Fatalf("merge lost duplicate identity/newer lifecycle/owner: %+v", snapshot)
	}
	for file, contents := range before {
		after, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(contents, after) {
			t.Fatalf("display snapshot rewrote %s: %v", file, err)
		}
	}
}
