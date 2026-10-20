package principal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func archiveFixture(t *testing.T, count int) (*Store, Job) {
	t.Helper()
	s := NewStore(t.TempDir())
	j, err := s.Create("long-lived job", testDefinition(t))
	if err != nil {
		t.Fatal(err)
	}
	// Populate a legacy-sized hot projection in one transaction; subsequent
	// production writes run the same migration/archive path.
	j, err = s.update(j.ID, func(j *Job) error {
		for i := range count {
			j.Runs = append(j.Runs, RunUsage{RunID: fmt.Sprintf("run-%06d", i), SourceRevision: 2, Status: "completed", InputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 5, OutputTokens: 7, SpentUSD: 1, UpdatedAt: s.now()})
		}
		return recount(j)
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, j
}

func TestArchivedJobHistoryBoundsManifestPreservesLifetimeAndEnumerates(t *testing.T) {
	s, j := archiveFixture(t, 1200)
	if len(j.Runs) != HotRunHistoryLimit || j.RunHistory.Count != 1200-HotRunHistoryLimit || j.RunCount() != 1200 {
		t.Fatalf("history not bounded: hot=%d history=%d count=%d", len(j.Runs), j.RunHistory.Count, j.RunCount())
	}
	info, err := os.Stat(filepath.Join(s.root, j.ID+".json"))
	if err != nil || info.Size() > 64<<10 {
		t.Fatalf("settled run history grew the manifest: %v %v", info, err)
	}
	files := 0
	if err = filepath.WalkDir(filepath.Join(s.root, j.ID, "run-history"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && filepath.Ext(path) == ".json" {
			files++
		}
		return nil
	}); err != nil || uint64(files) != j.RunHistory.Count {
		t.Fatalf("batch migration persisted intermediate obsolete trees: %d files, %d reachable, %v", files, j.RunHistory.Count, err)
	}
	j, err = NewStore(s.root).Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.InputTokens != 12000 || j.OutputTokens != 8400 || j.SpentUSD != 1200 {
		t.Fatalf("archival lost lifetime usage: %+v", j)
	}
	var runs []RunUsage
	cursor := ""
	for {
		page, next, err := NewStore(s.root).ListRunUsagePage(j.ID, cursor, 37)
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(runs) != 1200 {
		t.Fatalf("pagination lost/doubled historical runs: %d", len(runs))
	}
	for i, r := range runs {
		if r.RunID != fmt.Sprintf("run-%06d", i) {
			t.Fatalf("wrong history order at %d: %s", i, r.RunID)
		}
	}
	refresh, err := s.RefreshRuns(j.ID)
	if err != nil || len(refresh) != 0 {
		t.Fatalf("settled history still refreshed on every request: %v %v", refresh, err)
	}
}

func TestArchivedJobUsageLateCorrectionRevisionConflictAndUnresolvedReturn(t *testing.T) {
	s, j := archiveFixture(t, HotRunHistoryLimit+4)
	old, err := s.GetRunUsage(j.ID, "run-000000")
	if err != nil {
		t.Fatal(err)
	}
	// A registered old run remains idempotent even if the job is paused and
	// its lifetime budget is spent. It must not create a fresh empty entry.
	j, err = s.SetStatus(j.ID, Paused)
	if err != nil {
		t.Fatal(err)
	}
	j, err = NewStore(s.root).RegisterRun(j.Binding(), old.RunID)
	if err != nil || j.RunCount() != HotRunHistoryLimit+4 || j.SpentUSD != HotRunHistoryLimit+4 {
		t.Fatalf("archived registration was charged/registered again: %+v %v", j, err)
	}
	stale := old
	stale.SourceRevision = 1
	stale.InputTokens = 999
	if j, err = s.RecordRunUsage(j.Binding(), stale); err != nil || j.InputTokens != (HotRunHistoryLimit+4)*10 {
		t.Fatalf("stale archive correction changed totals: %+v %v", j, err)
	}
	conflict := old
	conflict.SpentUSD = 99
	if _, err = s.RecordRunUsage(j.Binding(), conflict); err == nil {
		t.Fatal("same archive revision accepted conflicting accounting")
	}
	corrected := old
	corrected.SourceRevision++
	corrected.InputTokens = 12
	corrected.SpentUSD = 2
	j, err = NewStore(s.root).RecordRunUsage(j.Binding(), corrected)
	if err != nil || j.InputTokens != (HotRunHistoryLimit+4)*10+10 || j.SpentUSD != HotRunHistoryLimit+5 {
		t.Fatalf("new archive correction not applied once: %+v %v", j, err)
	}
	for range 2 {
		if j, err = NewStore(s.root).RecordRunUsage(j.Binding(), corrected); err != nil {
			t.Fatal(err)
		}
	}
	if j.SpentUSD != HotRunHistoryLimit+5 {
		t.Fatal("duplicate corrected snapshot added spend")
	}
	corrected.SourceRevision++
	corrected.Status = "blocked"
	corrected.UnresolvedRequests = 1
	j, err = s.RecordRunUsage(j.Binding(), corrected)
	if err != nil || j.UnresolvedRequests != 1 || j.RunHistory.Count != 3 {
		t.Fatalf("late uncertainty did not return to hot accounting: %+v %v", j, err)
	}
	refresh, err := s.RefreshRuns(j.ID)
	if err != nil || len(refresh) != 1 || refresh[0].RunID != old.RunID {
		t.Fatalf("late uncertainty not returned for refreshing: %+v %v", refresh, err)
	}
	j, err = s.SetStatus(j.ID, Active)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewStore(s.root).ClaimExecution(j.Binding(), "next", "worker", time.Minute); !errors.Is(err, ErrUsageUnknown) {
		t.Fatalf("late unsettled history did not block dispatch: %v", err)
	}
	corrected.SourceRevision++
	corrected.Status = "completed"
	corrected.UnresolvedRequests = 0
	j, err = s.RecordRunUsage(j.Binding(), corrected)
	if err != nil || j.RunHistory.Count != 4 || j.UnresolvedRequests != 0 || j.RunCount() != HotRunHistoryLimit+4 {
		t.Fatalf("resettled run was lost/doubled: %+v %v", j, err)
	}
}

func TestArchiveRetainsEveryActiveBlockedAndUnknownRun(t *testing.T) {
	s, j := archiveFixture(t, HotRunHistoryLimit+2)
	for i, status := range []string{"active", "blocked", "completed", "cancelled"} {
		id := fmt.Sprintf("pending-%d", i)
		if _, err := s.RegisterRun(j.Binding(), id); err != nil {
			t.Fatal(err)
		}
		u := RunUsage{RunID: id, SourceRevision: 1, Status: status, UnresolvedRequests: 1, InputTokens: 15}
		if _, err := s.RecordRunUsage(j.Binding(), u); err != nil {
			t.Fatal(err)
		}
	}
	refresh, err := s.RefreshRuns(j.ID)
	if err != nil || len(refresh) != 4 {
		t.Fatalf("unfinished run disappeared: %+v %v", refresh, err)
	}
	j, err = NewStore(s.root).Get(j.ID)
	if err != nil || j.UnresolvedRequests != 4 || j.RunHistory.Count != 2 || len(j.Runs) != HotRunHistoryLimit+4 {
		t.Fatalf("unfinished accounting archived or lost: %+v %v", j, err)
	}
}

func TestArchiveNodeMissingCorruptAndSymlinkFailClosed(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			s, j := archiveFixture(t, HotRunHistoryLimit+2)
			p, err := s.historyPath(j, j.RunHistory.SHA256, false)
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing":
				err = os.Remove(p)
			case "corrupt":
				err = os.WriteFile(p, append(original, ' '), 0600)
			case "symlink":
				target := filepath.Join(t.TempDir(), "node.json")
				if err = os.WriteFile(target, original, 0600); err == nil {
					err = os.Remove(p)
				}
				if err == nil {
					err = os.Symlink(target, p)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = NewStore(s.root).Get(j.ID); err == nil {
				t.Fatal("damaged history root accepted")
			}
			if _, err = s.RegisterRun(j.Binding(), "run-000000"); err == nil {
				t.Fatal("damaged history became fresh registration")
			}
			if _, err = s.ClaimExecution(j.Binding(), "next", "worker", time.Minute); err == nil {
				t.Fatal("execution continued with damaged budget history")
			}
		})
	}
}

func TestUncommittedHistoryDoesNotChangeManifestAndLegacyUpgradesOnWrite(t *testing.T) {
	s, j := archiveFixture(t, HotRunHistoryLimit+2)
	p := filepath.Join(s.root, j.ID+".json")
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.GetRunUsage(j.ID, "run-000000")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a process stopping after syncing archive nodes but before
	// replacing the manifest. Only the previous root may be visible.
	old.SourceRevision++
	old.SpentUSD = 999
	if _, err = s.putArchived(j, j.RunHistory, old); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(p)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("uncommitted immutable nodes changed the job manifest")
	}
	j, err = NewStore(s.root).Get(j.ID)
	if err != nil || j.SpentUSD != HotRunHistoryLimit+2 {
		t.Fatalf("uncommitted archive spend became visible: %+v %v", j, err)
	}
	if removed, err := s.CompactRunHistory(j.ID); err != nil || removed == 0 {
		t.Fatalf("uncommitted obsolete nodes not cleaned up: %d %v", removed, err)
	}
	if got, err := s.GetRunUsage(j.ID, old.RunID); err != nil || got.SpentUSD != 1 {
		t.Fatalf("cleanup before manifest replacement damaged old root: %+v %v", got, err)
	}
	if j, err = s.RecordRunUsage(j.Binding(), old); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.CompactRunHistory(j.ID); err != nil || removed == 0 {
		t.Fatalf("committed old versions not cleaned up: %d %v", removed, err)
	}
	if got, err := NewStore(s.root).GetRunUsage(j.ID, old.RunID); err != nil || got.SpentUSD != 999 {
		t.Fatalf("cleanup after manifest replacement damaged new root: %+v %v", got, err)
	}
	if err = s.VerifyRunHistory(j.ID); err != nil {
		t.Fatal(err)
	}
	legacyStore := NewStore(t.TempDir())
	legacy, err := legacyStore.Create("legacy", testDefinition(t))
	if err != nil {
		t.Fatal(err)
	}
	legacy.Version = 1
	if err = legacyStore.write(legacy, false); err != nil {
		t.Fatal(err)
	}
	if _, err = legacyStore.Get(legacy.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = legacyStore.RegisterRun(legacy.Binding(), "new-run"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(legacyStore.root, legacy.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Job
	if err = json.Unmarshal(data, &persisted); err != nil || persisted.Version != SchemaVersion {
		t.Fatalf("old writer could accept updated archive schema: %+v %v", persisted, err)
	}
}

func TestMissingDeepArchiveNodeBlocksLookupAndCompaction(t *testing.T) {
	s, j := archiveFixture(t, HotRunHistoryLimit+10)
	root, err := s.readHistoryNode(j, j.RunHistory)
	if err != nil {
		t.Fatal(err)
	}
	child := root.Left
	if child.Count == 0 {
		child = root.Right
	}
	node, err := s.readHistoryNode(j, child)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.historyPath(j, child.SHA256, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetRunUsage(j.ID, node.Usage.RunID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("missing archived run became absent/fresh: %v", err)
	}
	if _, err = s.RegisterRun(j.Binding(), node.Usage.RunID); err == nil {
		t.Fatal("missing archived run registered as fresh")
	}
	if err = s.VerifyRunHistory(j.ID); err == nil {
		t.Fatal("full history audit ignored missing descendant")
	}
	if removed, err := s.CompactRunHistory(j.ID); err == nil || removed != 0 {
		t.Fatalf("cleanup proceeded before validating complete committed tree: %d %v", removed, err)
	}
}
