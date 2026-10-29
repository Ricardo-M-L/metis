package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sparseHomeManager(t *testing.T) (*Manager, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	return NewManager("sparse-home", home, ""), home
}

func TestSparseHomeEnableDoesNotSnapshotHomeAndIsSessionLocal(t *testing.T) {
	m, home := sparseHomeManager(t)
	other := NewManager("other", home, "")
	if err := os.Mkdir(filepath.Join(home, "unrelated"), 0); err != nil {
		t.Fatal(err)
	}
	state, err := m.Command(context.Background(), "on")
	if err != nil || !state.Enabled || !m.Sparse() || m.AutomaticDisabled() {
		t.Fatalf("enable: %+v %v", state, err)
	}
	if _, err := os.Stat(m.shadowDir); !os.IsNotExist(err) {
		t.Fatalf("enable scans or initializes home: %v", err)
	}
	if !other.ControlStatus().Enabled || !other.Sparse() {
		t.Fatal("other sessions must retain the automatic sparse default")
	}
	reopened := NewManager("sparse-home", home, "")
	if !reopened.ControlStatus().Enabled || reopened.AutomaticDisabled() {
		t.Fatal("enabled state did not persist")
	}
	if _, err := m.Command(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.RefreshControl(); err != nil || !reopened.AutomaticDisabled() {
		t.Fatalf("off sync: %v", err)
	}
}

func TestSparseBaselineExtendsWithoutReplacingEarlierPreimages(t *testing.T) {
	m, home := sparseHomeManager(t)
	if _, err := m.Command(context.Background(), "on"); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("first.txt", "first original")
	write("second.txt", "second original")
	write("unrelated.txt", "never snapshot")
	h, err := m.BaselinePathsContext(context.Background(), []string{"first.txt"}, "", "Write", "", "before turn")
	if err != nil {
		t.Fatal(err)
	}
	write("first.txt", "first edit")
	h, err = m.BaselinePathsContext(context.Background(), []string{"second.txt", "new.txt"}, h, "Write", "", "before turn")
	if err != nil {
		t.Fatal(err)
	}
	write("second.txt", "second edit")
	write("new.txt", "created")
	h2, err := m.BaselinePathsContext(context.Background(), []string{filepath.Join(home, "new.txt")}, h, "Edit", "", "before turn")
	if err != nil || h2 != h {
		t.Fatalf("repeated absent path changed baseline: %s %s %v", h, h2, err)
	}
	if err := m.RecordManagedPaths([]string{"first.txt", "second.txt", "new.txt"}); err != nil {
		t.Fatal(err)
	}
	states, err := m.CapturePathStates([]string{"first.txt", "second.txt", "new.txt"})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := m.treePaths(h)
	if err != nil || len(paths) != 2 {
		t.Fatalf("baseline scope: %+v %v", paths, err)
	}
	reopened := NewManager("sparse-home", home, "")
	if !reopened.Protects(filepath.Join(home, "first.txt")) || reopened.Protects(filepath.Join(home, "unrelated.txt")) {
		t.Fatal("registered paths not restored narrowly")
	}
	if err := reopened.RestorePathStates(h, states); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"first.txt": "first original", "second.txt": "second original", "unrelated.txt": "never snapshot"} {
		got, err := os.ReadFile(filepath.Join(home, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %s %v", name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new file was not removed: %v", err)
	}
}

func TestSparseBaselineRejectsUnsafeTargetsAndCancellation(t *testing.T) {
	m, home := sparseHomeManager(t)
	if _, err := m.Command(context.Background(), "on"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".", "../outside", ".metis/config.toml", "link/file.txt"} {
		if _, err := m.BaselinePathsContext(context.Background(), []string{path}, "", "Write", "", ""); err == nil {
			t.Errorf("accepted %s", path)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.BaselinePathsContext(ctx, []string{"file.txt"}, "", "Write", "", ""); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancelled snapshot: %v", err)
	}
}

func TestSparseBatchBackfillsHistoryUsingOriginalBlobs(t *testing.T) {
	m, home := sparseHomeManager(t)
	if _, err := m.Command(context.Background(), "on"); err != nil {
		t.Fatal(err)
	}
	first, second := filepath.Join(home, "first.txt"), filepath.Join(home, "later.txt")
	if err := os.WriteFile(first, []byte("original first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("original later"), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := m.BaselinePathsContext(context.Background(), []string{first}, "", "Write", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("edited first"), 0o600); err != nil {
		t.Fatal(err)
	}
	hashes, err := m.ExtendBaselinePathsContext(context.Background(), []string{second}, []string{"", old}, "Write", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("edited later"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordManagedPaths([]string{first, second}); err != nil {
		t.Fatal(err)
	}
	states, err := m.CapturePathStates([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RestorePathStates(hashes[1], states); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{first: "original first", second: "original later"} {
		body, err := os.ReadFile(path)
		if err != nil || string(body) != want {
			t.Fatalf("restore %s: %s %v", path, body, err)
		}
	}
}

func TestSparseAbsentMetadataIsImmutableAndMissingTargetsCannotBeRemoved(t *testing.T) {
	m, home := sparseHomeManager(t)
	if _, err := m.Command(context.Background(), "on"); err != nil {
		t.Fatal(err)
	}
	one, err := m.BaselinePathsContext(context.Background(), []string{"absent-one.txt"}, "", "Write", "", "")
	if err != nil {
		t.Fatal(err)
	}
	two, err := m.BaselinePathsContext(context.Background(), []string{"absent-two.txt"}, one, "Write", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if one == two {
		t.Fatal("captured absence changed without an immutable baseline identity")
	}
	oneSet, err := m.readSparseBaselineLocked(context.Background(), one)
	if err != nil || oneSet["absent-two.txt"] {
		t.Fatalf("old metadata overwritten: %+v %v", oneSet, err)
	}
	if err := os.WriteFile(filepath.Join(home, "absent-two.txt"), []byte("new file"), 0o600); err != nil {
		t.Fatal(err)
	}
	states, err := m.CapturePathStates([]string{"absent-two.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RestorePathStates(one, states); err == nil {
		t.Fatal("unknown preimage allowed deletion")
	}
	if body, err := os.ReadFile(filepath.Join(home, "absent-two.txt")); err != nil || string(body) != "new file" {
		t.Fatalf("missing captured path was modified: %s %v", body, err)
	}
	// A registry cannot claim a previously unknown absence by editing metadata.
	oneSet["absent-two.txt"] = true
	if err := m.writeSparseSet(m.sparseBaselineFile(one), oneSet); err != nil {
		t.Fatal(err)
	}
	if err := m.RestorePathStates(one, states); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("tampered captured set accepted: %v", err)
	}
}

func TestSparseDiffDoesNotIncludeUnregisteredSibling(t *testing.T) {
	m, home := sparseHomeManager(t)
	if _, err := m.Command(context.Background(), "on"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "known.txt"), []byte("known before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "unrelated.txt"), []byte("private sibling body"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := m.BaselinePathsContext(context.Background(), []string{"known.txt"}, "", "Edit", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "known.txt"), []byte("known after"), 0o600); err != nil {
		t.Fatal(err)
	}
	diff, err := m.Diff(h, "")
	if err != nil || !strings.Contains(diff, "known after") || strings.Contains(diff, "private sibling body") || strings.Contains(diff, "unrelated.txt") {
		t.Fatalf("sparse diff: %s %v", diff, err)
	}
}

func TestSparseDiffRejectsTrackedFileReplacedWithUnregisteredDirectory(t *testing.T) {
	m, home := sparseHomeManager(t)
	if _, err := m.Command(context.Background(), "on"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "known.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := m.BaselinePathsContext(context.Background(), []string{path}, "", "Edit", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "private-unregistered.txt"), []byte("must never enumerate this child"), 0o600); err != nil {
		t.Fatal(err)
	}
	diff, err := m.Diff(h, "")
	if err == nil || strings.Contains(diff, "must never enumerate this child") {
		t.Fatalf("unsafe sparse type change leaked: diff=%q err=%v", diff, err)
	}
}

func TestSparseDeletedTargetRestoresAndLiteralNameDiffDoesNotMatchSibling(t *testing.T) {
	m, home := sparseHomeManager(t)
	if _, err := m.Command(context.Background(), "on"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "file[1].txt")
	sibling := filepath.Join(home, "file1.txt")
	for name, body := range map[string]string{path: "original", sibling: "private sibling"} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h, err := m.BaselinePathsContext(context.Background(), []string{path}, "", "Edit", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordManagedPath(path); err != nil {
		t.Fatal(err)
	}
	states, err := m.CapturePathStates([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := m.Diff(h, "")
	if err != nil || !strings.Contains(diff, "original") || strings.Contains(diff, "private sibling") {
		t.Fatalf("literal deleted target diff: %s %v", diff, err)
	}
	if err := m.RestorePathStates(h, states); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "original" {
		t.Fatalf("deleted restore: %s %v", body, err)
	}
}

func TestSparseMetadataWriteFailureDoesNotReturnUsableBaseline(t *testing.T) {
	m, _ := sparseHomeManager(t)
	if _, err := m.Command(context.Background(), "on"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	err := m.initShadowRepoContext(context.Background())
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.shadowDir, ".git", "metis-sparse-baselines"), []byte("block metadata directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := m.BaselinePathsContext(context.Background(), []string{"new.txt"}, "", "Write", "", "")
	if err == nil || hash != "" {
		t.Fatalf("metadata failure advertised baseline: %s %v", hash, err)
	}
}
