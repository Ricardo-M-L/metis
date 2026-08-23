package artifact

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestDefaultStoreUsesMetisHomeAndPrivateRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)

	store, err := DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "artifacts")
	want, err = filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}
	if store.Root() != want {
		t.Fatalf("root = %q, want %q", store.Root(), want)
	}
	assertMode(t, want, 0o700)
}

func TestStoreLifecycleIsVersionedAndSessionOwned(t *testing.T) {
	store := newTestStore(t)
	rawV1 := `<!doctype html><html><head><style>.ok{color:red}</style><script>alert(1)</script></head><body onload="steal()"><h1>First</h1><a href="https://example.com">external</a><a href="#local">local</a><iframe src="https://example.com"></iframe></body></html>`
	created, err := store.Create("session-a", " Dashboard ", rawV1)
	if err != nil {
		t.Fatal(err)
	}
	if created.SessionID != "session-a" || created.Title != "Dashboard" || created.CurrentVersion != 1 || len(created.Versions) != 1 {
		t.Fatalf("unexpected manifest: %+v", created)
	}

	bodyV1, versionV1, err := store.ReadVersion("session-a", created.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if versionV1.Number != 1 || versionV1.Size != int64(len(bodyV1)) {
		t.Fatalf("unexpected version: %+v (body=%d)", versionV1, len(bodyV1))
	}
	for _, forbidden := range []string{"<script", "onload=", "https://example.com", "<iframe"} {
		if bytes.Contains(bytes.ToLower(bodyV1), []byte(forbidden)) {
			t.Fatalf("sanitized HTML retained %q:\n%s", forbidden, bodyV1)
		}
	}
	for _, wanted := range []string{"First", `href="#local"`, "color: red"} {
		if !bytes.Contains(bodyV1, []byte(wanted)) {
			t.Fatalf("sanitized HTML lost %q:\n%s", wanted, bodyV1)
		}
	}

	if _, err := store.Get("session-b", created.ID); !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("cross-session Get error = %v, want ErrOwnerMismatch", err)
	}
	if _, _, err := store.ReadVersion("session-b", created.ID, 0); !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("cross-session ReadVersion error = %v, want ErrOwnerMismatch", err)
	}
	if _, err := store.Update("session-b", created.ID, "stolen", "<p>stolen</p>"); !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("cross-session Update error = %v, want ErrOwnerMismatch", err)
	}
	if err := store.Delete("session-b", created.ID); !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("cross-session Delete error = %v, want ErrOwnerMismatch", err)
	}
	if _, err := store.List(""); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("unowned List error = %v, want ErrInvalidSession", err)
	}
	if got, err := store.List("session-b"); err != nil || len(got) != 0 {
		t.Fatalf("session-b list = %+v, %v; want empty", got, err)
	}

	updated, err := store.Update("session-a", created.ID, "", "<main><h1>Second</h1></main>")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "Dashboard" || updated.CurrentVersion != 2 || len(updated.Versions) != 2 {
		t.Fatalf("unexpected updated manifest: %+v", updated)
	}
	againV1, _, err := store.ReadVersion("session-a", created.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(againV1, bodyV1) {
		t.Fatal("updating an artifact changed immutable version 1")
	}
	bodyV2, _, err := store.ReadVersion("session-a", created.ID, 0)
	if err != nil || !bytes.Contains(bodyV2, []byte("Second")) {
		t.Fatalf("current version body = %q, err=%v", bodyV2, err)
	}

	items, err := store.List("session-a")
	if err != nil || len(items) != 1 || items[0].CurrentVersion != 2 {
		t.Fatalf("session-a list = %+v, err=%v", items, err)
	}
	// Returned manifests must not alias the store or one another.
	items[0].Versions[0].SHA256 = "mutated"
	got, err := store.Get("session-a", created.ID)
	if err != nil || got.Versions[0].SHA256 == "mutated" {
		t.Fatalf("manifest copy leaked caller mutation: %+v, %v", got, err)
	}

	if err := store.Delete("session-a", created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("session-a", created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete error = %v, want ErrNotFound", err)
	}
}

func TestStoreDeleteSessionOnlyDeletesExactOwner(t *testing.T) {
	store := newTestStore(t)
	a1, err := store.Create("session-a", "one", "<p>one</p>")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := store.Create("session-a", "two", "<p>two</p>")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Create("session-b", "other", "<p>other</p>")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteSession("session-a"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a1.ID, a2.ID} {
		if _, err := store.Get("session-a", id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session-a artifact %s survived: %v", id, err)
		}
	}
	if _, err := store.Get("session-b", b.ID); err != nil {
		t.Fatalf("session-b artifact was deleted: %v", err)
	}
}

func TestStorePermissionsDigestAndUnsafeLayout(t *testing.T) {
	store := newTestStore(t)
	item, err := store.Create("session-a", "private", "<p>trusted</p>")
	if err != nil {
		t.Fatal(err)
	}

	dir := store.artifactDir(item.ID)
	versions := filepath.Join(dir, "versions")
	manifest := filepath.Join(dir, "manifest.json")
	version := store.versionPath(item.ID, 1)
	for _, path := range []string{store.Root(), dir, versions} {
		assertMode(t, path, 0o700)
	}
	for _, path := range []string{manifest, version} {
		assertMode(t, path, 0o600)
	}

	if err := os.WriteFile(version, []byte("<p>tampered</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReadVersion("session-a", item.ID, 1); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("digest mismatch error = %v, want ErrUnsafeFile", err)
	}

	// Even digest-valid content must remain in a private regular file on
	// platforms where FileMode represents POSIX permissions. Windows exposes
	// ACL-backed files through synthetic permission bits instead.
	if runtime.GOOS != "windows" {
		clean, err := SanitizeHTML("<p>trusted</p>")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(version, []byte(clean), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(version, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ReadVersion("session-a", item.ID, 1); !errors.Is(err, ErrUnsafeFile) {
			t.Fatalf("public version mode error = %v, want ErrUnsafeFile", err)
		}
	}
}

func TestPrivatePermissionValidationUsesPlatformSemantics(t *testing.T) {
	if !hasPrivatePermissions(0o600, 0o600) || !hasPrivatePermissions(0o700, 0o700) {
		t.Fatal("exact private modes must always be accepted")
	}
	if runtime.GOOS == "windows" {
		if !hasPrivatePermissions(0o666, 0o600) || !hasPrivatePermissions(0o777, 0o700) {
			t.Fatal("Windows synthetic FileMode bits must defer to ACL-backed storage")
		}
		return
	}
	if hasPrivatePermissions(0o644, 0o600) || hasPrivatePermissions(0o755, 0o700) {
		t.Fatal("non-Windows stores must reject group/other permission bits")
	}
}

func TestStoreRejectsSymlinkRootAndVersionsDirectory(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := NewStore(link); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("symlink root error = %v, want ErrUnsafeFile", err)
	}

	store := newTestStore(t)
	item, err := store.Create("session-a", "safe", "<p>safe</p>")
	if err != nil {
		t.Fatal(err)
	}
	versions := filepath.Join(store.artifactDir(item.ID), "versions")
	moved := filepath.Join(base, "moved-versions")
	if err := os.Rename(versions, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, versions); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := store.ReadVersion("session-a", item.ID, 1); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("symlink versions directory error = %v, want ErrUnsafeFile", err)
	}
}

func TestStoreExportIsVerifiedAtomicAndNoReplace(t *testing.T) {
	store := newTestStore(t)
	item, err := store.Create("session-a", "export", "<h1>export me</h1>")
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := store.ReadVersion("session-a", item.ID, 1)
	if err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "artifact.html")
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			errs <- store.Export("session-a", item.ID, 1, destination)
		}()
	}
	close(start)
	var successes, alreadyExists int
	for i := 0; i < 2; i++ {
		err := <-errs
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAlreadyExists):
			alreadyExists++
		default:
			t.Fatalf("Export error = %v", err)
		}
	}
	if successes != 1 || alreadyExists != 1 {
		t.Fatalf("concurrent Export successes=%d alreadyExists=%d, want 1/1", successes, alreadyExists)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("exported bytes mismatch: %q != %q", got, want)
	}
	assertMode(t, destination, 0o600)

	wrongOwner := filepath.Join(t.TempDir(), "wrong-owner.html")
	if err := store.Export("session-b", item.ID, 1, wrongOwner); !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("cross-session Export error = %v, want ErrOwnerMismatch", err)
	}
	if _, err := os.Lstat(wrongOwner); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-session Export created destination: %v", err)
	}
}

func TestStoreConcurrentUpdatesAllocateEveryVersionOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	first, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	item, err := first.Create("session-a", "counter", "<p>zero</p>")
	if err != nil {
		t.Fatal(err)
	}

	const updates = 32
	start := make(chan struct{})
	errCh := make(chan error, updates)
	var wg sync.WaitGroup
	for i := 0; i < updates; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			store := first
			if i%2 == 1 {
				store = second
			}
			_, err := store.Update("session-a", item.ID, "", fmt.Sprintf("<p>%d</p>", i+1))
			errCh <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := first.Get("session-a", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentVersion != updates+1 || len(got.Versions) != updates+1 {
		t.Fatalf("manifest after concurrent updates: current=%d versions=%d", got.CurrentVersion, len(got.Versions))
	}
	for number := 1; number <= updates+1; number++ {
		if _, meta, err := second.ReadVersion("session-a", item.ID, number); err != nil || meta.Number != number {
			t.Fatalf("version %d: meta=%+v err=%v", number, meta, err)
		}
	}
}

func TestStoreRejectsOversizeAndInvalidInputs(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Create("session-a", "large", strings.Repeat("x", MaxHTMLBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize Create error = %v, want ErrTooLarge", err)
	}
	if _, err := store.Create("", "title", "<p>x</p>"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("empty session error = %v, want ErrInvalidSession", err)
	}
	if _, err := store.Create("session-a", "", "<p>x</p>"); !errors.Is(err, ErrInvalidTitle) {
		t.Fatalf("empty title error = %v, want ErrInvalidTitle", err)
	}
	if _, err := store.Get("session-a", "../escape"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("traversal id error = %v, want ErrInvalidID", err)
	}
	if err := store.Export("session-a", "valid-id", 0, "relative.html"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("relative export error = %v, want ErrInvalidPath", err)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %#o, want %#o", path, got, want)
	}
}
