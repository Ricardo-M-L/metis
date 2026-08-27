package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/memdir"
)

func topicTestMemo(name, body string) []byte {
	return []byte(fmt.Sprintf("---\nname: %s\ndescription: %s topic\ntype: project\n---\n\n%s\n", name, name, body))
}

func TestTopicOwnershipKeepsSessionDeletionExact(t *testing.T) {
	root := t.TempDir()
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	pathA := filepath.Join(root, "project_a.md")
	pathB := filepath.Join(root, "project_b.md")
	sourceA := TopicSource{SessionID: "session-a", MessageID: "a-1", Scope: "project", Confidence: 0.8}
	sourceB := TopicSource{SessionID: "session-b", MessageID: "b-1", Scope: "project", Confidence: 0.8}
	if err := manager.CommitTopic(context.Background(), TopicMutation{Path: pathA, Content: topicTestMemo("alpha", "alpha fact"), Source: sourceA}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CommitTopic(context.Background(), TopicMutation{Path: pathA, Content: topicTestMemo("stolen", "session B overwrite"), Source: sourceB}); !errors.Is(err, ErrTopicOwnership) {
		t.Fatalf("cross-session Write error=%v, want ErrTopicOwnership", err)
	}
	if err := manager.CommitTopic(context.Background(), TopicMutation{
		Path: pathA, Content: topicTestMemo("stolen", "session B edit"), Source: sourceB,
		ExpectedSHA256: TopicContentSHA256(original),
	}); !errors.Is(err, ErrTopicOwnership) {
		t.Fatalf("cross-session Edit error=%v, want ErrTopicOwnership", err)
	}
	if err := manager.CommitTopic(context.Background(), TopicMutation{Path: pathB, Content: topicTestMemo("bravo", "bravo fact"), Source: sourceB}); err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteSession("session-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pathA); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session A topic survived deletion: %v", err)
	}
	if _, err := os.Stat(pathB); err != nil {
		t.Fatalf("session B topic was deleted with A: %v", err)
	}
	if err := manager.DeleteSession("session-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pathB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session B topic survived its deletion: %v", err)
	}
}

func TestTopicCommitRejectsClaimingUnattributedLegacyFile(t *testing.T) {
	root := t.TempDir()
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "legacy.md")
	if err := os.WriteFile(path, topicTestMemo("legacy", "unattributed"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = manager.CommitTopic(context.Background(), TopicMutation{
		Path: path, Content: topicTestMemo("legacy", "claimed"),
		Source: TopicSource{SessionID: "new-session"},
	})
	if !errors.Is(err, ErrTopicOwnership) {
		t.Fatalf("legacy claim error=%v, want ErrTopicOwnership", err)
	}
}

func TestRemoveTopicEmptySourceIsExplicitUserAdminDelete(t *testing.T) {
	root := t.TempDir()
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "owned.md")
	if err := manager.CommitTopic(context.Background(), TopicMutation{
		Path: path, Content: topicTestMemo("owned", "owned fact"),
		Source: TopicSource{SessionID: "owner"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.RemoveTopic(context.Background(), path, "other-session"); !errors.Is(err, ErrTopicOwnership) {
		t.Fatalf("foreign auto-memory removal error=%v, want ErrTopicOwnership", err)
	}
	if err := manager.RemoveTopic(context.Background(), path, ""); err != nil {
		t.Fatalf("explicit user-admin removal: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("admin removal left topic: %v", err)
	}
}

func TestMaintainTopicsDoesNotCrossAttributeConcurrentTopic(t *testing.T) {
	root := t.TempDir()
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "session_a.md")
	if err := manager.CommitTopic(context.Background(), TopicMutation{
		Path: path, Content: topicTestMemo("session a", "owned by A"),
		Source: TopicSource{SessionID: "session-a", MessageID: "a-1"},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = manager.MaintainTopics(context.Background(), TopicMaintenanceRequest{
		Touched: []string{filepath.Base(path)},
		Source:  TopicSource{SessionID: "session-b", MessageID: "b-1"},
		Now:     time.Now(),
	})
	if !errors.Is(err, ErrTopicOwnership) {
		t.Fatalf("cross-session maintenance error=%v, want ErrTopicOwnership", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fm, _, err := memdir.ParseFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if fm.OriginSessionID != "session-a" || fm.SourceMessageID != "a-1" {
		t.Fatalf("maintenance cross-attributed topic: %+v", fm)
	}
}

func TestTopicEditRevisionConflict(t *testing.T) {
	root := t.TempDir()
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "revision.md")
	source := TopicSource{SessionID: "owner"}
	if err := manager.CommitTopic(context.Background(), TopicMutation{Path: path, Content: topicTestMemo("revision", "v1"), Source: source}); err != nil {
		t.Fatal(err)
	}
	stale, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CommitTopic(context.Background(), TopicMutation{Path: path, Content: topicTestMemo("revision", "v2"), Source: source}); err != nil {
		t.Fatal(err)
	}
	err = manager.CommitTopic(context.Background(), TopicMutation{
		Path: path, Content: topicTestMemo("revision", "stale overwrite"), Source: source,
		ExpectedSHA256: TopicContentSHA256(stale),
	})
	if !errors.Is(err, ErrTopicConflict) {
		t.Fatalf("stale edit error=%v, want ErrTopicConflict", err)
	}
}

func TestTopicTwoManagersWriteDeleteRaceNeverResurrects(t *testing.T) {
	root := t.TempDir()
	writer, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	deleter, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		sessionID := fmt.Sprintf("race-%d", i)
		path := filepath.Join(root, fmt.Sprintf("race_%d.md", i))
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var writeErr, deleteErr error
		go func() {
			defer wg.Done()
			<-start
			writeErr = writer.CommitTopic(context.Background(), TopicMutation{
				Path: path, Content: topicTestMemo("race", "concurrent write"),
				Source: TopicSource{SessionID: sessionID},
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			deleteErr = deleter.DeleteSession(sessionID)
		}()
		close(start)
		wg.Wait()
		if writeErr != nil && !errors.Is(writeErr, ErrSessionDeleted) {
			t.Fatalf("iteration %d write: %v", i, writeErr)
		}
		if deleteErr != nil {
			t.Fatalf("iteration %d delete: %v", i, deleteErr)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("iteration %d resurrected topic: %v", i, err)
		}
	}
}

func TestTopicCommitSubprocessHelper(t *testing.T) {
	root := os.Getenv("METIS_TOPIC_HELPER_ROOT")
	if root == "" {
		return
	}
	ready := os.Getenv("METIS_TOPIC_HELPER_READY")
	goFile := os.Getenv("METIS_TOPIC_HELPER_GO")
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(goFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for subprocess barrier")
		}
		time.Sleep(time.Millisecond)
	}
	err = manager.CommitTopic(context.Background(), TopicMutation{
		Path:    filepath.Join(root, "subprocess.md"),
		Content: topicTestMemo("subprocess", "cross process write"),
		Source:  TopicSource{SessionID: "subprocess-session"},
	})
	if err != nil && !errors.Is(err, ErrSessionDeleted) {
		t.Fatal(err)
	}
}

func TestTopicSubprocessWriteDeleteRaceNeverResurrects(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "memory")
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(base, "ready")
	goFile := filepath.Join(base, "go")
	cmd := exec.Command(os.Args[0], "-test.run=^TestTopicCommitSubprocessHelper$", "-test.count=1")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.Env = append(os.Environ(),
		"METIS_TOPIC_HELPER_ROOT="+root,
		"METIS_TOPIC_HELPER_READY="+ready,
		"METIS_TOPIC_HELPER_GO="+goFile,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("timed out waiting for helper readiness")
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.WriteFile(goFile, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteSession("subprocess-session"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("subprocess commit: %v\n%s", err, output.String())
	}
	path := filepath.Join(root, "subprocess.md")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subprocess write resurrected topic: %v", err)
	}
	if err := manager.CommitTopic(context.Background(), TopicMutation{
		Path: path, Content: topicTestMemo("subprocess", "late retry"),
		Source: TopicSource{SessionID: "subprocess-session"},
	}); !errors.Is(err, ErrSessionDeleted) {
		t.Fatalf("durable tombstone did not reject retry: %v", err)
	}
}
