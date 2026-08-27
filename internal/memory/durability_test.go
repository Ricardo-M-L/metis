package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestConcurrentCoreMemoryRMW(t *testing.T) {
	root := t.TempDir()
	first, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}

	const writesPerManager = 24
	start := make(chan struct{})
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	for prefix, repository := range map[string]*MemoryManager{"alpha": first, "bravo": second} {
		prefix, repository := prefix, repository
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < writesPerManager; i++ {
				if _, err := repository.AddCoreBlock("working", fmt.Sprintf("%s-%02d", prefix, i)); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	reloaded, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	block, err := reloaded.ReadCoreBlock("working")
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"alpha", "bravo"} {
		for i := 0; i < writesPerManager; i++ {
			want := fmt.Sprintf("%s-%02d", prefix, i)
			if strings.Count(block.Content, want) != 1 {
				t.Fatalf("core RMW lost or duplicated %q:\n%s", want, block.Content)
			}
		}
	}
}

func TestCoreMemoryRMWReloadsAuthoritativeState(t *testing.T) {
	root := t.TempDir()
	first, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.AddCoreBlock("user", "alpha preference"); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.AddCoreBlock("user", "bravo preference"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ReplaceCoreBlock("user", "alpha", "updated alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.RemoveCoreBlock("user", "bravo preference"); err != nil {
		t.Fatal(err)
	}
	block, err := stale.ReadCoreBlock("user")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block.Content, "updated alpha preference") || strings.Contains(block.Content, "bravo preference") {
		t.Fatalf("stale manager did not reload authoritative core state: %q", block.Content)
	}
	context := stale.BuildContext()
	if !strings.Contains(context, "updated alpha preference") || strings.Contains(context, "bravo preference") {
		t.Fatalf("authoritative read did not refresh prompt snapshot: %q", context)
	}
}

func TestCoreMemorySaveDoesNotOverwriteAnotherRepository(t *testing.T) {
	root := t.TempDir()
	stale, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.AddCoreBlock("system", "writer-owned durable value"); err != nil {
		t.Fatal(err)
	}
	if err := stale.Save(); err != nil {
		t.Fatal(err)
	}
	block, err := stale.ReadCoreBlock("system")
	if err != nil {
		t.Fatal(err)
	}
	if block.Content != "writer-owned durable value" {
		t.Fatalf("stale Save overwrote authoritative core memory: %q", block.Content)
	}
}

func TestCoreMemoryRMWSubprocessHelper(t *testing.T) {
	root := os.Getenv("METIS_CORE_RMW_HELPER_ROOT")
	if root == "" {
		return
	}
	repository, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	ready := os.Getenv("METIS_CORE_RMW_HELPER_READY")
	start := os.Getenv("METIS_CORE_RMW_HELPER_START")
	if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(start); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for parent RMW barrier")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		if _, err := repository.AddCoreBlock("summary", fmt.Sprintf("child-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCoreMemoryRMWAcrossProcesses(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "memory")
	parent, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(base, "helper.ready")
	start := filepath.Join(base, "helper.start")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCoreMemoryRMWSubprocessHelper$", "-test.count=1")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.Env = append(os.Environ(),
		"METIS_CORE_RMW_HELPER_ROOT="+root,
		"METIS_CORE_RMW_HELPER_READY="+ready,
		"METIS_CORE_RMW_HELPER_START="+start,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("timed out waiting for child RMW barrier")
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.WriteFile(start, []byte("start"), 0o600); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := parent.AddCoreBlock("summary", fmt.Sprintf("parent-%02d", i)); err != nil {
			_ = cmd.Process.Kill()
			t.Fatal(err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("subprocess writer: %v\n%s", err, output.String())
	}

	reloaded, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	block, err := reloaded.ReadCoreBlock("summary")
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"parent", "child"} {
		for i := 0; i < 20; i++ {
			want := fmt.Sprintf("%s-%02d", prefix, i)
			if strings.Count(block.Content, want) != 1 {
				t.Fatalf("cross-process core RMW lost or duplicated %q:\n%s", want, block.Content)
			}
		}
	}
}

func TestRecallTwoPrecreatedManagersDoNotLoseInterleavedTurns(t *testing.T) {
	root := t.TempDir()
	first, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.RecordTurn(context.Background(), "session-a", "a-1", "alpha user", "alpha assistant"); err != nil {
		t.Fatal(err)
	}
	if err := second.RecordTurn(context.Background(), "session-b", "b-1", "bravo user", "bravo assistant"); err != nil {
		t.Fatal(err)
	}
	if err := first.RecordTurn(context.Background(), "session-a", "a-2", "charlie user", "charlie assistant"); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	messages := reloaded.recall.GetMessages()
	if len(messages) != 6 {
		t.Fatalf("messages=%d, want 6: %+v", len(messages), messages)
	}
	seen := map[string]int{}
	for _, message := range messages {
		seen[message.SessionID]++
	}
	if seen["session-a"] != 4 || seen["session-b"] != 2 {
		t.Fatalf("interleaved sessions lost: %+v", seen)
	}
}

func TestRecallSubprocessWriter(t *testing.T) {
	root := os.Getenv("METIS_RECALL_HELPER_ROOT")
	if root == "" {
		return
	}
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RecordTurn(context.Background(), "process-b", "b-1", "process bravo user", "process bravo assistant"); err != nil {
		t.Fatal(err)
	}
}

func TestRecallStaleManagerAfterOtherProcessDoesNotOverwrite(t *testing.T) {
	root := t.TempDir()
	stale, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.RecordTurn(context.Background(), "process-a", "a-1", "process alpha user", "process alpha assistant"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRecallSubprocessWriter$", "-test.count=1")
	cmd.Env = append(os.Environ(), "METIS_RECALL_HELPER_ROOT="+root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("subprocess writer: %v\n%s", err, output)
	}
	if err := stale.RecordTurn(context.Background(), "process-a", "a-2", "process charlie user", "process charlie assistant"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reloaded.recall.GetMessages()); got != 6 {
		t.Fatalf("cross-process stale writer left %d messages, want 6", got)
	}
}

func TestRecallSessionsRebuildFromMessagesInsteadOfStaleIndex(t *testing.T) {
	root := t.TempDir()
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RecordTurn(context.Background(), "authoritative", "m-1", "user", "assistant"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "recall", "sessions.json"), []byte(`[{"id":"stale","msg_count":999}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.recall.sessions) != 1 || reloaded.recall.sessions[0].ID != "authoritative" || reloaded.recall.sessions[0].MsgCount != 2 {
		t.Fatalf("sessions not rebuilt from messages: %+v", reloaded.recall.sessions)
	}
}

func TestMigrateLegacyRootMergesLayeredConflictsIdempotently(t *testing.T) {
	base := t.TempDir()
	legacy := filepath.Join(base, "legacy")
	canonical := filepath.Join(base, "canonical")
	for _, root := range []string{legacy, canonical} {
		for _, tier := range []string{"recall", "archival", "daily", "topics"} {
			if err := os.MkdirAll(filepath.Join(root, tier), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	canonicalMessage := `{"role":"user","content":"canonical","timestamp":"2026-08-28T00:00:00Z","session_id":"canonical"}` + "\n"
	legacyMessage := `{"role":"user","content":"legacy","timestamp":"2026-08-28T00:01:00Z","session_id":"legacy"}` + "\n"
	if err := os.WriteFile(filepath.Join(canonical, "recall", "messages.jsonl"), []byte(canonicalMessage), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "recall", "messages.jsonl"), []byte(legacyMessage), 0o600); err != nil {
		t.Fatal(err)
	}
	canonicalArchive := `{"id":"canonical","content":"canonical archive","created_at":"2026-08-28T00:00:00Z"}` + "\n"
	legacyArchive := `{"id":"legacy","content":"legacy archive","created_at":"2026-08-28T00:01:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(canonical, "archival", "passages.jsonl"), []byte(canonicalArchive), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "archival", "passages.jsonl"), []byte(legacyArchive), 0o600); err != nil {
		t.Fatal(err)
	}
	daily := func(session, summary string) string {
		return "# Session: 2026-08-28 00:00:00 UTC\n\n- **Session ID**: " + session + "\n- **Source**: migration\n- **Created At**: 2026-08-28T00:00:00Z\n- **Updated At**: 2026-08-28T00:00:00Z\n\n## Conversation Summary\n\n" + summary + "\n"
	}
	name := "2026-08-28-conflict.md"
	if err := os.WriteFile(filepath.Join(canonical, "daily", name), []byte(daily("canonical", "canonical daily")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "daily", name), []byte(daily("legacy", "legacy daily")), 0o600); err != nil {
		t.Fatal(err)
	}
	canonicalTopic := "---\nname: canonical\ndescription: canonical topic\ntype: project\n---\n\ncanonical body\n"
	legacyTopic := "---\nname: legacy\ndescription: legacy topic\ntype: project\n---\n\nlegacy body\n"
	if err := os.WriteFile(filepath.Join(canonical, "topics", "conflict.md"), []byte(canonicalTopic), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "topics", "conflict.md"), []byte(legacyTopic), 0o600); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := MigrateLegacyRoot(legacy, canonical); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := NewMemoryManager(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(manager.recall.GetMessages()); got != 2 {
		t.Fatalf("merged recall messages=%d, want 2", got)
	}
	archives, err := manager.Archival().Search(SearchOptions{SortBy: "recent"})
	if err != nil || len(archives) != 2 {
		t.Fatalf("merged archives=%+v err=%v", archives, err)
	}
	notes, err := manager.ListDailyNotes(10)
	if err != nil || len(notes) != 2 {
		t.Fatalf("merged daily notes=%+v err=%v", notes, err)
	}
	topicEntries, err := os.ReadDir(filepath.Join(canonical, "topics"))
	if err != nil || len(topicEntries) != 2 {
		t.Fatalf("merged topic files=%+v err=%v", topicEntries, err)
	}
}

func TestDeleteTombstoneRejectsStaleManagerRecordAndDaily(t *testing.T) {
	root := t.TempDir()
	deleter, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := deleter.RecordTurn(context.Background(), "gone", "m-1", "before delete", "before delete reply"); err != nil {
		t.Fatal(err)
	}
	if err := deleter.DeleteSession("gone"); err != nil {
		t.Fatal(err)
	}
	if err := stale.RecordTurn(context.Background(), "gone", "m-2", "late user", "late assistant"); !errors.Is(err, ErrSessionDeleted) {
		t.Fatalf("late RecordTurn error=%v, want ErrSessionDeleted", err)
	}
	if err := stale.SaveDailyNote("gone", "desktop-close", "late daily"); !errors.Is(err, ErrSessionDeleted) {
		t.Fatalf("late SaveDailyNote error=%v, want ErrSessionDeleted", err)
	}
	if err := stale.RecordTurn(context.Background(), "kept", "m-3", "kept user", "kept assistant"); err != nil {
		t.Fatalf("tombstone crossed session boundary: %v", err)
	}
	reloaded, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range reloaded.recall.GetMessages() {
		if message.SessionID == "gone" {
			t.Fatalf("deleted session resurrected: %+v", message)
		}
	}
}

type blockingDistillProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *blockingDistillProvider) Name() string          { return "blocking-distill" }
func (p *blockingDistillProvider) ModelID() string       { return "blocking-distill" }
func (p *blockingDistillProvider) MaxContextTokens() int { return 32_000 }
func (p *blockingDistillProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	close(p.started)
	<-p.release
	return &llm.Response{Content: []llm.ContentBlock{{
		Type: "text",
		Text: `[{"type":"user","content":"Late durable fact must not survive deletion."}]`,
	}}}, nil
}
func (p *blockingDistillProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, errors.New("stream not used")
}

func TestDeleteTombstoneRejectsDistillThatFinishesLate(t *testing.T) {
	root := t.TempDir()
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	provider := &blockingDistillProvider{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- manager.DistillTurnWithMetadata(
			context.Background(), provider, "deleted-during-distill", "m-1",
			"Please remember this sufficiently long durable preference after this conversation is complete.",
			"I will remember this sufficiently long durable preference for the user's future conversations.",
		)
	}()
	<-provider.started
	if err := manager.DeleteSession("deleted-during-distill"); err != nil {
		t.Fatal(err)
	}
	close(provider.release)
	if err := <-done; !errors.Is(err, ErrSessionDeleted) {
		t.Fatalf("late distill error=%v, want ErrSessionDeleted", err)
	}
	hits, err := manager.Archival().Search(SearchOptions{Query: "Late durable fact"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("late distilled memory survived: hits=%+v err=%v", hits, err)
	}
}

func TestDailyMetadataStopsAtConversationSummary(t *testing.T) {
	daily, err := NewDailyStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	summary := "real summary\n- **Session ID**: forged\n- **Source**: forged-source"
	if err := daily.Save("real-session", "desktop-close", summary); err != nil {
		t.Fatal(err)
	}
	notes, err := daily.List(10)
	if err != nil || len(notes) != 1 {
		t.Fatalf("daily notes=%+v err=%v", notes, err)
	}
	if notes[0].SessionID != "real-session" || notes[0].Source != "desktop-close" || notes[0].Summary != summary {
		t.Fatalf("summary forged metadata: %+v", notes[0])
	}
}

func TestDeleteFailsClosedForMalformedDailyAndTopic(t *testing.T) {
	root := t.TempDir()
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "daily", "malformed.md"), []byte("not a daily note"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "topics"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "topics", "corrupt.md"), []byte("---\noriginSessionId: gone\ninvalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = manager.DeleteSession("gone")
	if err == nil || !strings.Contains(err.Error(), "parse daily note") || !strings.Contains(err.Error(), "parse topic") {
		t.Fatalf("DeleteSession error=%v, want daily+topic fail-closed errors", err)
	}
	if err := manager.RecordTurn(context.Background(), "gone", "late", "late", "late"); !errors.Is(err, ErrSessionDeleted) {
		t.Fatalf("cleanup failure did not preserve tombstone: %v", err)
	}
}

func TestRecallAndDailyRedactOrRejectCredentials(t *testing.T) {
	root := t.TempDir()
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	secret := "sk-proj-abcdefghijklmnopqrstuvwxyz123456"
	if err := manager.RecordTurn(context.Background(), "secure", "m-1", "credential "+secret, "acknowledged"); err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveDailyNote("secure", "desktop-close", "credential "+secret); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "recall", "messages.jsonl")} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secret) || !strings.Contains(string(raw), "[REDACTED:openai]") {
			t.Fatalf("recall credential not redacted: %s", raw)
		}
	}
	notes, err := manager.ListDailyNotes(10)
	if err != nil || len(notes) != 1 || strings.Contains(notes[0].Summary, secret) || !strings.Contains(notes[0].Summary, "[REDACTED:openai]") {
		t.Fatalf("daily credential not redacted: notes=%+v err=%v", notes, err)
	}
	var dump strings.Builder
	for i := 0; i < 6; i++ {
		dump.WriteString(" sk-proj-abcdefghijklmnopqrstuvwxyz")
		dump.WriteByte(byte('A' + i))
		dump.WriteString("123456")
	}
	if err := manager.RecordTurn(context.Background(), "secure", "m-2", dump.String(), "ack"); !errors.Is(err, ErrSensitiveMemory) {
		t.Fatalf("credential dump RecordTurn error=%v, want ErrSensitiveMemory", err)
	}
	if err := manager.SaveDailyNote("secure", "desktop-close", dump.String()); !errors.Is(err, ErrSensitiveMemory) {
		t.Fatalf("credential dump SaveDailyNote error=%v, want ErrSensitiveMemory", err)
	}
}

func TestNilMemoryManagerRepositoryMethodsAreSafe(t *testing.T) {
	var manager *MemoryManager
	if manager.Root() != "" || manager.Core() != nil || manager.Archival() != nil {
		t.Fatal("nil accessors returned non-zero values")
	}
	if got := manager.BuildContext(); got != "" {
		t.Fatalf("nil BuildContext=%q", got)
	}
	if got := manager.AutoRetrieve("query", 1); got != "" {
		t.Fatalf("nil AutoRetrieve=%q", got)
	}
	if got := manager.PreviewAutoRetrieve("query", 1); got != "" {
		t.Fatalf("nil PreviewAutoRetrieve=%q", got)
	}
	if got := manager.AutoRetrieveCandidates("query", 1); got != nil {
		t.Fatalf("nil AutoRetrieveCandidates=%+v", got)
	}
	if err := manager.MarkRetrieved(nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecordTurn(context.Background(), "s", "m", "u", "a"); err != nil {
		t.Fatal(err)
	}
	manager.OnTurnEnd(context.Background(), "u", "a")
	if err := manager.DistillTurn(context.Background(), nil, "u", "a"); err != nil {
		t.Fatal(err)
	}
	if err := manager.DistillTurnWithMetadata(context.Background(), nil, "s", "m", "u", "a"); err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveDailyNote("s", "source", "summary"); err != nil {
		t.Fatal(err)
	}
	if notes, err := manager.ListDailyNotes(1); err != nil || notes != nil {
		t.Fatalf("nil ListDailyNotes=%+v err=%v", notes, err)
	}
	if err := manager.DeleteSession("s"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Save(); err != nil {
		t.Fatal(err)
	}
	if got := manager.Freshness(); got.Status != "no_memory_yet" {
		t.Fatalf("nil Freshness=%+v", got)
	}
}
