package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A content-identical snapshot must not rewrite every mirror file. Besides
// unnecessary IO, rewriting makes Git rehash the complete workspace each time.
func TestSnapUnchangedPreservesMirrorMtime(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	if err := os.WriteFile(filepath.Join(cwd, "unchanged.txt"), []byte("same bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snap("Bash", "before", "baseline"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.shadowDir, "unchanged.txt")
	stamp := time.Unix(1234567890, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if hash, err := m.Snap("Bash", "same", "unchanged"); err != nil || hash != "" {
		t.Fatalf("unchanged snapshot: hash=%q err=%v", hash, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(stamp) {
		t.Fatalf("unchanged mirror rewritten: mtime=%s want=%s", info.ModTime(), stamp)
	}
}

func TestSnapshotContentValidationAndModeChanges(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	path := filepath.Join(cwd, "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := m.BaselineContext(context.Background(), "Bash", "first", "baseline")
	if err != nil {
		t.Fatal(err)
	}
	stats := m.LastSnapshotStats()
	if stats.Files != 1 || stats.Bytes != 6 || stats.CopiedFiles != 1 || !m.HasBaseline() {
		t.Fatalf("first scan stats=%+v baseline=%t", stats, m.HasBaseline())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if changed, err := m.ChangedPathsContext(context.Background(), first); err != nil || len(changed) != 1 || changed[0] != "file.txt" {
		t.Fatalf("same-size same-mtime delta=%v err=%v", changed, err)
	}
	second, err := m.BaselineContext(context.Background(), "Bash", "second", "same size and timestamp")
	if err != nil || second == first || m.LastSnapshotStats().CopiedFiles != 1 {
		t.Fatalf("same-size same-mtime edit missed: hash=%q err=%v stats=%+v", second, err, m.LastSnapshotStats())
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	third, err := m.BaselineContext(context.Background(), "Bash", "mode", "executable")
	if err != nil || third == second {
		t.Fatalf("mode edit missed: hash=%q err=%v", third, err)
	}
	// Tampering with the mirror without changing source size/mtime also must
	// be repaired; content comparison does not trust either file's metadata.
	mirror := filepath.Join(m.shadowDir, "file.txt")
	if err := os.WriteFile(mirror, []byte("bad!!!"), 0o700); err != nil {
		t.Fatal(err)
	}
	if hash, err := m.BaselineContext(context.Background(), "Bash", "repair", "repair mirror"); err != nil || hash != third {
		t.Fatalf("mirror repair changed baseline: hash=%q err=%v", hash, err)
	}
	body, err := os.ReadFile(mirror)
	if err != nil || string(body) != "after!" {
		t.Fatalf("mirror repair body=%q err=%v", body, err)
	}
}

func TestChangedPathsAttributesModeOnlyEditAndRestoresIt(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	path := filepath.Join(cwd, "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := m.BaselineContext(context.Background(), "Bash", "before", "before chmod")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	changed, err := m.ChangedPathsContext(context.Background(), baseline)
	if err != nil || len(changed) != 1 || changed[0] != "script.sh" {
		t.Fatalf("mode-only mutation missing from attribution: changed=%v err=%v", changed, err)
	}
	states, err := m.CapturePathStates(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RestorePathStates(baseline, states); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode()&0o111 != 0 {
		t.Fatalf("mode-only rewind did not restore non-executable baseline: info=%v err=%v", info, err)
	}
}

func TestBaselineContextReusesVerifiedHeadWithoutNewCommit(t *testing.T) {
	skipIfNoGit(t)
	m, _ := freshManager(t)
	if m.HasBaseline() {
		t.Fatal("uninitialized manager reports a baseline")
	}
	first, err := m.BaselineContext(context.Background(), "Bash", "one", "turn one")
	if err != nil || first == "" {
		t.Fatalf("first baseline=%q err=%v", first, err)
	}
	second, err := m.BaselineContext(context.Background(), "Bash", "two", "turn two")
	if err != nil || second != first {
		t.Fatalf("unchanged baseline=%q want=%q err=%v", second, first, err)
	}
	if hash, err := m.SnapContext(context.Background(), "Bash", "legacy", "legacy contract"); err != nil || hash != "" {
		t.Fatalf("legacy unchanged Snap hash=%q err=%v", hash, err)
	}
	if list, err := m.List(10); err != nil || len(list) != 1 {
		t.Fatalf("unchanged baseline made redundant commits: count=%d err=%v", len(list), err)
	}
	// Budget selection must not block behind an ongoing snapshot.
	m.mu.Lock()
	done := make(chan bool, 1)
	go func() { done <- m.HasBaseline() }()
	select {
	case has := <-done:
		if !has {
			t.Error("completed baseline not reported")
		}
	case <-time.After(time.Second):
		t.Error("HasBaseline blocked on snapshot lock")
	}
	m.mu.Unlock()
}

func TestSnapshotProgressCancellationIsDiagnosableAndRetryable(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	if err := os.WriteFile(filepath.Join(cwd, "private-name.txt"), []byte("private-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stages []string
	ctx = WithProgress(ctx, func(progress SnapshotProgress) {
		stages = append(stages, progress.Stage)
		if progress.Stage == "copy" {
			cancel()
		}
	})
	if hash, err := m.BaselineContext(ctx, "Bash", "cancel", "cancel before copy"); hash != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot hash=%q err=%v", hash, err)
	} else {
		var scanErr *SnapshotError
		if !errors.As(err, &scanErr) || scanErr.Stage != "copy" || scanErr.Stats.Files != 1 || scanErr.Stats.Bytes != 12 {
			t.Fatalf("missing phase/size diagnostics: %v", err)
		}
		if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), cwd) {
			t.Fatalf("diagnostic disclosed path or contents: %v", err)
		}
	}
	if len(stages) == 0 || m.HasBaseline() {
		t.Fatalf("cancelled first copy reported baseline: stages=%v", stages)
	}
	stats := m.LastSnapshotStats()
	if stats.StageDurations["inventory"] <= 0 || stats.StageDurations["copy"] <= 0 {
		t.Fatalf("missing stage durations: %+v", stats)
	}
	if hash, err := m.BaselineContext(context.Background(), "Bash", "retry", "retry"); hash == "" || err != nil {
		t.Fatalf("retry after cancelled copy hash=%q err=%v", hash, err)
	}
}

func TestParallelFileProcessingJoinsCancelledWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	files := make([]liveFile, 100)
	for i := range files {
		files[i].rel = fmt.Sprintf("file-%d", i)
	}
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var completed atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := processLiveFiles(ctx, files, newScanObserver(ctx), func(file liveFile) fileResult {
			entered <- struct{}{}
			<-release
			completed.Add(1)
			return fileResult{rel: file.rel, copied: true}
		})
		done <- err
	}()
	for i := 0; i < 8; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("workers did not reach deterministic barrier")
		}
	}
	cancel()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || completed.Load() != 8 {
			t.Fatalf("returned before all in-flight writes joined: completed=%d err=%v", completed.Load(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled workers were not joined")
	}
}

func TestInventoryAndReadsConfineSymlinkReplacements(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	inside := filepath.Join(cwd, "inside")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inside, "file"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cwd, "outside-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	tree, err := m.inventoryTree(context.Background(), root, newScanObserver(context.Background()))
	if err != nil || len(tree.files) != 1 || tree.files[0].rel != "inside/file" {
		t.Fatalf("inventory followed a symlink: files=%v err=%v", tree.files, err)
	}
	if err := os.Rename(inside, filepath.Join(cwd, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, inside); err != nil {
		t.Fatal(err)
	}
	if body, err := readLiveFile(root, tree.files[0]); err == nil || body != nil {
		t.Fatalf("raced directory replacement escaped source root: body length=%d err=%v", len(body), err)
	}
}

func TestInventoryRejectsDirectoryReplacedBySkippedTreeSymlink(t *testing.T) {
	m, cwd := freshManager(t)
	ordinary := filepath.Join(cwd, "ordinary")
	skipped := filepath.Join(cwd, ".metis")
	for _, path := range []string{ordinary, skipped} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.Lstat(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Remove(ordinary); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".metis", ordinary); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if child, err := openInventoryDirectory(root, "ordinary", expected); err == nil {
		child.Close()
		t.Fatal("directory replacement followed a symlink into a skipped tree")
	}
	if tree, err := m.inventoryTree(context.Background(), root, newScanObserver(context.Background())); err != nil || len(tree.files) != 0 || len(tree.dirs) != 0 {
		t.Fatalf("inventory accepted symlink alias of skipped directory: tree=%+v err=%v", tree, err)
	}
}

func TestBaselineIncludesInventoryFilesIgnoredBySourceGitignore(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	if err := os.WriteFile(filepath.Join(cwd, ".gitignore"), []byte("*.tgz\nvendor/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cwd, "vendor"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"archive.tgz", "vendor/retained.go"} {
		if err := os.WriteFile(filepath.Join(cwd, rel), []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	baseline, err := m.BaselineContext(context.Background(), "Bash", "before", "before readonly query")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := m.ChangedPathsContext(context.Background(), baseline)
	if err != nil || len(changed) != 0 {
		t.Fatalf("pre-existing ignored files misattributed as newly created: paths=%v err=%v", changed, err)
	}
	path := filepath.Join(cwd, "archive.tgz")
	if err := os.WriteFile(path, []byte("modified"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err = m.ChangedPathsContext(context.Background(), baseline)
	if err != nil || len(changed) != 1 || changed[0] != "archive.tgz" {
		t.Fatalf("modified inventory file missing: paths=%v err=%v", changed, err)
	}
	states, err := m.CapturePathStates(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RestorePathStates(baseline, states); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "original" {
		t.Fatalf("ignored file baseline was not restorable: body=%q err=%v", body, err)
	}
}

func TestBaselinePreservesBytesDespiteSourceAttributes(t *testing.T) {
	skipIfNoGit(t)
	for _, tc := range []struct {
		name       string
		attributes string
		body       string
	}{
		{"text", "*.txt text eol=lf\n", "first\r\nsecond\r\n"},
		{"ident", "*.txt ident\n", "$Id: expanded-user-content$\n"},
		{"encoding", "*.txt working-tree-encoding=UTF-16LE\n", "a\x00b\x00\n\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, cwd := freshManager(t)
			if err := os.WriteFile(filepath.Join(cwd, ".gitattributes"), []byte(tc.attributes), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "original.txt"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			baseline, err := m.BaselineContext(context.Background(), "Bash", "before", "raw bytes")
			if err != nil {
				t.Fatal(err)
			}
			stored, err := m.gitOutputBytes("show", baseline+":original.txt")
			if err != nil || string(stored) != tc.body {
				t.Fatalf("snapshot transformed original bytes: stored=%q want=%q err=%v", stored, tc.body, err)
			}
			if paths, err := m.ChangedPathsContext(context.Background(), baseline); err != nil || len(paths) != 0 {
				t.Fatalf("source attributes created false changes: paths=%v err=%v", paths, err)
			}
		})
	}
}

func TestExistingNormalizedShadowIndexIsMigratedToRawBytes(t *testing.T) {
	skipIfNoGit(t)
	m, cwd := freshManager(t)
	body := "first\r\nsecond\r\n"
	if err := os.WriteFile(filepath.Join(cwd, ".gitattributes"), []byte("*.txt text eol=lf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "original.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BaselineContext(context.Background(), "Bash", "initial", "initialize"); err != nil {
		t.Fatal(err)
	}
	// Model a pre-fix session whose mirrored bytes are already CRLF but whose
	// index and HEAD contain LF. The mirror's size/mtime remain unchanged.
	if err := os.Remove(filepath.Join(m.shadowDir, ".git", "info", "attributes")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.rawSnapshotFormatPath()); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1234567890, 0)
	if err := os.Chtimes(filepath.Join(m.shadowDir, "original.txt"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := m.git("add", "--renormalize", "."); err != nil {
		t.Fatal(err)
	}
	if err := m.git("commit", "-q", "-m", "legacy normalized baseline"); err != nil {
		t.Fatal(err)
	}
	reopened := NewManager(m.sessionID, cwd, filepath.Dir(m.shadowDir))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = WithProgress(ctx, func(progress SnapshotProgress) {
		if progress.Stage == "migrate-index" {
			cancel()
		}
	})
	if _, err := reopened.BaselineContext(ctx, "Bash", "cancel-migration", "cancel raw migration"); !errors.Is(err, context.Canceled) {
		t.Fatalf("migration cancellation not preserved: %v", err)
	}
	if _, err := os.Stat(reopened.rawSnapshotFormatPath()); !os.IsNotExist(err) {
		t.Fatalf("cancelled migration prematurely marked raw index: %v", err)
	}
	reopened = NewManager(m.sessionID, cwd, filepath.Dir(m.shadowDir))
	baseline, err := reopened.BaselineContext(context.Background(), "Bash", "reopen", "reopen legacy session")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := reopened.gitOutputBytes("show", baseline+":original.txt")
	if err != nil || string(stored) != body {
		t.Fatalf("legacy cached index retained normalized bytes: stored=%q want=%q err=%v", stored, body, err)
	}
	var migratedAgain bool
	reopened = NewManager(m.sessionID, cwd, filepath.Dir(m.shadowDir))
	ctx = WithProgress(context.Background(), func(progress SnapshotProgress) {
		migratedAgain = migratedAgain || progress.Stage == "migrate-index"
	})
	if _, err := reopened.BaselineContext(ctx, "Bash", "again", "already migrated"); err != nil || migratedAgain {
		t.Fatalf("raw index migration unnecessarily repeated: migrated=%t err=%v", migratedAgain, err)
	}
}
