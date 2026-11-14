package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUsageLedgerDoesNotRewriteCorpus(t *testing.T) {
	root := t.TempDir()
	topicPath := filepath.Join(root, "ledger-topic.md")
	if err := os.WriteFile(topicPath, []byte("---\nname: ledger topic\ndescription: amber workstation\ntype: user\n---\n\nAmber workstation has quiet fans.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mm, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := mm.Archival().Insert(Passage{ID: "ledger-archive", Content: "Amber archive records quiet fans.", UseCount: 7, LastUsedAt: "2020-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	hits, err := mm.Search(SearchOptions{SortBy: "recent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits=%d, want 2", len(hits))
	}
	paths := []string{topicPath, filepath.Join(root, "archival", "passages.jsonl"), filepath.Join(root, "archival", "index.json")}
	before := make([][]byte, len(paths))
	for i, path := range paths {
		before[i], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	builds := mm.retrievalCache.builds
	if err := mm.MarkRetrieved(hits); err != nil {
		t.Fatal(err)
	}
	for i, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before[i], after) {
			t.Fatalf("usage rewrote corpus file %s", filepath.Base(path))
		}
	}
	if _, err := mm.Search(SearchOptions{SortBy: "recent"}); err != nil {
		t.Fatal(err)
	}
	if mm.retrievalCache.builds != builds {
		t.Fatalf("usage rebuilt corpus: before=%d after=%d", builds, mm.retrievalCache.builds)
	}
}

func TestUsageLedgerBaselineIdempotencyAndRestart(t *testing.T) {
	root := t.TempDir()
	mm, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Passage{
		{ID: "used", Content: "Cedar archive", UseCount: 9, LastUsedAt: "2020-01-01T00:00:00Z"},
		{ID: "other", Content: "Pine archive"},
	} {
		if err := mm.Archival().Insert(p); err != nil {
			t.Fatal(err)
		}
	}
	selection := []Passage{{ID: "used"}, {ID: "used"}}
	if err := mm.MarkRetrievedEvent("one-request", selection); err != nil {
		t.Fatal(err)
	}
	if err := mm.MarkRetrievedEvent("one-request", selection[:1]); err != nil {
		t.Fatal(err)
	}
	assertArchiveUsage(t, mm, "used", 10, true)
	hits, err := mm.Search(SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mm.decorateUsage(hits); err != nil {
		t.Fatal(err)
	}
	for _, p := range hits {
		if p.ID == "used" && p.UseCount != 10 {
			t.Fatalf("double overlay=%d", p.UseCount)
		}
	}
	if err := mm.MarkRetrievedEvent("one-request", []Passage{{ID: "other"}}); err == nil {
		t.Fatal("event identity conflict accepted")
	}
	assertArchiveUsage(t, mm, "other", 0, false)
	reopened, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.MarkRetrievedEvent("one-request", selection); err != nil {
		t.Fatal(err)
	}
	assertArchiveUsage(t, reopened, "used", 10, true)
	if err := reopened.MarkRetrievedEvent("next-request", selection); err != nil {
		t.Fatal(err)
	}
	assertArchiveUsage(t, mm, "used", 11, true)
	data, err := os.ReadFile(filepath.Join(root, usageLedgerPath))
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(data, []byte{'\n'}); got != 2 {
		t.Fatalf("transactions=%d, want 2", got)
	}
	for _, path := range []string{filepath.Join(root, "usage"), filepath.Join(root, usageLedgerPath)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Fatalf("mode=%o, want %o", info.Mode().Perm(), want)
		}
	}
}

func TestUsageLedgerConcurrentManagers(t *testing.T) {
	root := t.TempDir()
	first, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Archival().Insert(Passage{ID: "shared", Content: "Fir deployment"}); err != nil {
		t.Fatal(err)
	}
	second, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			manager := first
			if i%2 == 0 {
				manager = second
			}
			errs <- manager.MarkRetrievedEvent(fmt.Sprintf("request-%d", i/2), []Passage{{ID: "shared"}})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertArchiveUsage(t, first, "shared", 6, true)
	assertArchiveUsage(t, second, "shared", 6, true)
}

func TestUsageLedgerRejectsReplacedIdentityAndWrongWorkspace(t *testing.T) {
	t.Run("same id changed identity", func(t *testing.T) {
		for _, changed := range []string{"source", "created", "scope"} {
			t.Run(changed, func(t *testing.T) {
				root := t.TempDir()
				mm, err := NewMemoryManager(root)
				if err != nil {
					t.Fatal(err)
				}
				original := Passage{ID: "reused", Content: "Birch original", CreatedAt: "2020-01-01T00:00:00Z", SourceSessionID: "original-owner", Scope: "project-one"}
				if err := mm.Archival().Insert(original); err != nil {
					t.Fatal(err)
				}
				replacement := original
				switch changed {
				case "source":
					replacement.SourceSessionID = "replacement-owner"
				case "created":
					replacement.CreatedAt = "2021-01-01T00:00:00Z"
				case "scope":
					replacement.Scope = "project-two"
				}
				data, err := json.Marshal(replacement)
				if err != nil {
					t.Fatal(err)
				}
				if err := atomicWriteFile(filepath.Join(root, "archival", "passages.jsonl"), string(data)+"\n", 0o600); err != nil {
					t.Fatal(err)
				}
				if err := mm.MarkRetrievedEvent("stale-receipt", []Passage{original}); err == nil {
					t.Fatal("replaced passage accepted stale identity")
				}
				assertArchiveUsage(t, mm, original.ID, 0, false)
				if err := mm.MarkRetrievedEvent("legacy-empty-receipt", []Passage{{ID: original.ID}}); err != nil {
					t.Fatal(err)
				}
				assertArchiveUsage(t, mm, original.ID, 1, true)
			})
		}
	})
	t.Run("wrong workspace", func(t *testing.T) {
		root := t.TempDir()
		first, err := NewMemoryManagerForWorkspace(root, filepath.Join(t.TempDir(), "one"))
		if err != nil {
			t.Fatal(err)
		}
		second, err := NewMemoryManagerForWorkspace(root, filepath.Join(t.TempDir(), "two"))
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Archival().Insert(Passage{ID: "scoped", Type: TypeProject, Content: "Birch project record"}); err != nil {
			t.Fatal(err)
		}
		hits, err := first.Search(SearchOptions{})
		if err != nil || len(hits) != 1 {
			t.Fatalf("first search count=%d err=%v", len(hits), err)
		}
		if err := second.MarkRetrievedEvent("wrong-target", hits); err == nil {
			t.Fatal("wrong workspace accepted receipt")
		}
		assertArchiveUsage(t, first, "scoped", 0, false)
	})
}

func TestUsageLedgerTopicBaselineSurvivesEdit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "baseline-topic.md")
	content := "---\nname: Hazel\ndescription: workstation preference\ntype: user\nuse_count: 5\nlast_used_at: 2020-01-01T00:00:00Z\n---\n\nHazel workstation has quiet fans.\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mm, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := mm.Search(SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].UseCount != 5 || hits[0].LastUsedAt != "2020-01-01T00:00:00Z" {
		t.Fatalf("legacy baseline=%+v", hits)
	}
	if err := mm.MarkRetrievedEvent("topic-use", hits); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(content, "quiet fans", "quiet cooling fans", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := mm.Search(SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 1 || updated[0].UseCount != 6 || updated[0].LastUsedAt == "2020-01-01T00:00:00Z" {
		t.Fatalf("edited topic lost usage=%+v", updated)
	}
	reopened, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	hits, err = reopened.Search(SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].UseCount != 6 {
		t.Fatalf("restart lost topic usage=%+v", hits)
	}
}

func TestUsageLedgerTopicScopeChangeIsolatesStatistics(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "scope-topic.md")
	content := "---\nname: Hazel\ndescription: workstation preference\ntype: project\nscope: project-one\n---\n\nHazel workstation has quiet fans.\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mm, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := mm.Search(SearchOptions{})
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits=%d err=%v", len(hits), err)
	}
	if err := mm.MarkRetrievedEvent("first-scope-use", hits); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(content, "scope: project-one", "scope: project-two", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	hits, err = mm.Search(SearchOptions{})
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits=%d err=%v", len(hits), err)
	}
	if hits[0].UseCount != 0 || hits[0].LastUsedAt != "" {
		t.Fatalf("scope inherited another workspace's usage: count=%d last=%q", hits[0].UseCount, hits[0].LastUsedAt)
	}
	if err := mm.MarkRetrievedEvent("second-scope-use", hits); err != nil {
		t.Fatal(err)
	}
	hits, err = mm.Search(SearchOptions{})
	if err != nil || len(hits) != 1 || hits[0].UseCount != 1 {
		t.Fatalf("new scope usage=%+v err=%v", hits, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	hits, err = mm.Search(SearchOptions{})
	if err != nil || len(hits) != 1 || hits[0].UseCount != 1 {
		t.Fatalf("original scope usage=%+v err=%v", hits, err)
	}
}

func TestUsageLedgerRecoversTornTailAtomically(t *testing.T) {
	for _, tail := range []string{"{\"version\":1", "{\"version\":1}\n"} {
		t.Run(fmt.Sprintf("tail-%d", len(tail)), func(t *testing.T) {
			root := t.TempDir()
			mm, err := NewMemoryManager(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"one", "two"} {
				if err := mm.Archival().Insert(Passage{ID: id, Content: "Spruce record " + id}); err != nil {
					t.Fatal(err)
				}
			}
			selection := []Passage{{ID: "one"}, {ID: "two"}}
			if err := mm.MarkRetrievedEvent("before-tail", selection); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, usageLedgerPath)
			valid, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString(tail); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			assertArchiveUsage(t, mm, "one", 1, true)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, append(append([]byte(nil), valid...), []byte(tail)...)) {
				t.Fatal("read mutated damaged tail")
			}
			if err := mm.MarkRetrievedEvent("after-tail", selection); err != nil {
				t.Fatal(err)
			}
			assertArchiveUsage(t, mm, "one", 2, true)
			assertArchiveUsage(t, mm, "two", 2, true)
			data, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(data, valid) || bytes.Count(data, []byte{'\n'}) != 2 {
				t.Fatal("tail repair changed committed transactions")
			}
		})
	}
}

func TestUsageLedgerRejectsInteriorCorruptionAndUnsafePath(t *testing.T) {
	t.Run("interior corruption", func(t *testing.T) {
		root := t.TempDir()
		mm, err := NewMemoryManager(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := mm.Archival().Insert(Passage{ID: "one", Content: "Oak record"}); err != nil {
			t.Fatal(err)
		}
		if err := mm.MarkRetrievedEvent("valid", []Passage{{ID: "one"}}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, usageLedgerPath)
		valid, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		corrupt := append([]byte("{broken}\n"), valid...)
		if err := os.WriteFile(path, corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := mm.Archival().Search(SearchOptions{}); err == nil {
			t.Fatal("interior corruption accepted by read")
		}
		if err := mm.MarkRetrievedEvent("later", []Passage{{ID: "one"}}); err == nil {
			t.Fatal("interior corruption accepted by write")
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(corrupt, after) {
			t.Fatal("corruption was silently discarded")
		}
	})
	t.Run("ledger symlink", func(t *testing.T) {
		root := t.TempDir()
		mm, err := NewMemoryManager(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := mm.Archival().Insert(Passage{ID: "one", Content: "Maple record"}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, usageLedgerPath)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Skip(err)
		}
		if err := mm.MarkRetrievedEvent("unsafe", []Passage{{ID: "one"}}); err == nil {
			t.Fatal("symlink ledger accepted")
		}
		after, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != "outside" {
			t.Fatal("ledger escaped root")
		}
	})
}

func TestUsageLedgerDeletedObjectsStayDeleted(t *testing.T) {
	root := t.TempDir()
	mm, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	p := Passage{ID: "owned", Content: "Willow session record", SourceSessionID: "owner"}
	if err := mm.Archival().Insert(p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "owned-topic.md")
	if err := mm.CommitTopic(context.Background(), TopicMutation{Path: path, Content: []byte("---\nname: willow\ndescription: session record\ntype: project\n---\n\nWillow session record.\n"), Source: TopicSource{SessionID: "owner"}}); err != nil {
		t.Fatal(err)
	}
	hits, err := mm.Search(SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mm.MarkRetrievedEvent("before-delete", hits); err != nil {
		t.Fatal(err)
	}
	if err := mm.DeleteSession("owner"); err != nil {
		t.Fatal(err)
	}
	if err := mm.MarkRetrievedEvent("stale-use", hits); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := reopened.Search(SearchOptions{})
	if err != nil || len(remaining) != 0 {
		t.Fatalf("deleted objects returned: count=%d err=%v", len(remaining), err)
	}
	data, err := os.ReadFile(filepath.Join(root, usageLedgerPath))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte{'\n'}) != 1 {
		t.Fatal("deleted objects appended new usage events")
	}
}

func TestUsageLedgerSurvivesKilledWriter(t *testing.T) {
	root := t.TempDir()
	mm, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := mm.Archival().Insert(Passage{ID: "durable", Content: "Aspen durable record"}); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestUsageLedgerKilledWriterHelper$")
	command.Env = append(os.Environ(), "METIS_USAGE_LEDGER_TEST_ROOT="+root)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() { _ = command.Process.Kill() })
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("writer exited before acknowledgement: %v %s", err, output.String())
		case <-deadline.C:
			t.Fatal("writer acknowledgement timed out")
		case <-tick.C:
			if _, err := os.Stat(filepath.Join(root, "usage-ack")); err != nil {
				continue
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			<-done
			reopened, err := NewMemoryManager(root)
			if err != nil {
				t.Fatal(err)
			}
			assertArchiveUsage(t, reopened, "durable", 1, true)
			if err := reopened.MarkRetrievedEvent("killed-process-request", []Passage{{ID: "durable"}}); err != nil {
				t.Fatal(err)
			}
			assertArchiveUsage(t, reopened, "durable", 1, true)
			return
		}
	}
}

func TestUsageLedgerKilledWriterHelper(t *testing.T) {
	root := os.Getenv("METIS_USAGE_LEDGER_TEST_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	if !strings.Contains(filepath.ToSlash(root), "/TestUsageLedgerSurvivesKilledWriter") {
		t.Fatal("expected isolated test directory")
	}
	mm, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := mm.MarkRetrievedEvent("killed-process-request", []Passage{{ID: "durable"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usage-ack"), []byte("durable"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}
