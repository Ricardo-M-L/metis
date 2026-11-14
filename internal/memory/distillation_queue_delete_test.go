package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/memdir"
)

func TestDeleteSessionScrubsQueuedDistillationWithoutWorker(t *testing.T) {
	manager, err := NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeJob := func(id, session string) string {
		job := map[string]any{"version": 1, "id": id, "session_id": session, "source_message_id": "message", "status": "running", "attempts": 7,
			"user_message": "private user fact " + session, "assistant_message": "private assistant text " + session, "last_failure": "private provider detail", "lease_until": "2030-01-01T00:00:00Z"}
		data, _ := json.Marshal(job)
		relative := filepath.Join(distillationQueueDir, id+".json")
		if err := memdir.AtomicWritePrivateFile(manager.Root(), relative, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(manager.Root(), relative)
	}
	deletedPath := writeJob(strings.Repeat("a", 64), "deleted-session")
	otherPath := writeJob(strings.Repeat("b", 64), "other-session")
	if err := manager.DeleteSession("deleted-session"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(deletedPath)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	if job["status"] != "revoked" || job["attempts"] != float64(7) || job["source_message_id"] != "message" {
		t.Fatalf("deletion envelope=%v", job)
	}
	for _, forbidden := range []string{"private user fact", "private assistant text", "private provider detail", "lease_until", "user_message", "assistant_message"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("delete retained queued %q", forbidden)
		}
	}
	other, err := os.ReadFile(otherPath)
	if err != nil || !strings.Contains(string(other), "private user fact other-session") {
		t.Fatalf("other source was scrubbed: %s err=%v", other, err)
	}
	if err := CheckSourceSessionAtRoot(manager.Root(), "deleted-session"); err != ErrSessionDeleted {
		t.Fatalf("tombstone preflight=%v", err)
	}
}
