package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDeleteSubagentDisplaySidecarsFollowCurrentOwner(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeader("parent-a", "model", ""); err != nil {
		t.Fatal(err)
	}
	for _, subdir := range []string{"subagents", "subagent-usage", "subagent-activity"} {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, child := range []struct{ id, owner string }{{"owned", "parent-a"}, {"resumed", "parent-b"}} {
		line := fmt.Sprintf(`{"type":"header","header":{"id":%q,"sub_agent_of":%q}}`+"\n", child.id, child.owner)
		if err := os.WriteFile(filepath.Join(dir, "subagents", child.id+".jsonl"), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, subdir := range []string{"subagent-usage", "subagent-activity"} {
			if err := os.WriteFile(filepath.Join(dir, subdir, child.id+".jsonl"), []byte("private receipts\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Delete("parent-a"); err != nil {
		t.Fatal(err)
	}
	for _, subdir := range []string{"subagents", "subagent-usage", "subagent-activity"} {
		if _, err := os.Stat(filepath.Join(dir, subdir, "owned.jsonl")); !os.IsNotExist(err) {
			t.Errorf("owned %s was retained: %v", subdir, err)
		}
		if _, err := os.Stat(filepath.Join(dir, subdir, "resumed.jsonl")); err != nil {
			t.Errorf("cross-parent resumed %s was removed: %v", subdir, err)
		}
	}
}

func TestDeleteSubagentSidecarFailureKeepsOwnershipHeader(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHeader("parent", "model", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(dir, "subagents", "child.jsonl")
	if err := os.WriteFile(transcriptPath, []byte("{\"type\":\"header\",\"header\":{\"id\":\"child\",\"sub_agent_of\":\"parent\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blockedPath := filepath.Join(dir, "subagent-usage", "child.jsonl")
	if err := os.MkdirAll(blockedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blockedPath, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("parent"); err == nil {
		t.Fatal("sidecar failure was reported as successful deletion")
	}
	for _, path := range []string{transcriptPath, store.path("parent")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("retry ownership header lost: %s: %v", path, err)
		}
	}
}
