package spill

import (
	"os"
	"strings"
	"sync"
	"testing"
)

func TestStoreRepeatedProviderIDPreservesEachResult(t *testing.T) {
	dir := t.TempDir()
	firstBody := strings.Repeat("first-output ", 5000)
	secondBody := strings.Repeat("second-output ", 5000)
	first, err := Store(dir, "provider-reused-id", firstBody)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Store(dir, "provider-reused-id", secondBody)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		result *Result
		body   string
	}{{"first", first, firstBody}, {"second", second, secondBody}} {
		got, err := os.ReadFile(test.result.Path)
		if err != nil || string(got) != test.body {
			t.Errorf("%s preview and recovery disagree: recovered current output=%v err=%v", test.name, string(got) == test.body, err)
		}
	}
	if first.Path == second.Path {
		t.Error("distinct outputs share a mutable recovery path")
	}
	third, err := Store(dir, "provider-reused-id", secondBody)
	if err != nil || third.Path != second.Path {
		t.Fatalf("identical output did not reuse its immutable recovery path: result=%v err=%v", third, err)
	}
}

func TestStoreSanitizedIDCollisionPreservesBothResults(t *testing.T) {
	dir := t.TempDir()
	first, err := Store(dir, "provider/id", "first body")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Store(dir, "provider?id", "second body")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(second.Path)
	if err != nil || string(got) != "second body" {
		t.Fatalf("sanitized ID collision returned another output: recovered=%q err=%v", got, err)
	}
	got, err = os.ReadFile(first.Path)
	if err != nil || string(got) != "first body" {
		t.Fatalf("publishing second result damaged first recovery: recovered=%q err=%v", got, err)
	}
}

func TestStoreRejectsCorruptedExistingResult(t *testing.T) {
	dir := t.TempDir()
	r, err := Store(dir, "corrupt-id", "expected body")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.Path, []byte("different body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result, err := Store(dir, "corrupt-id", "expected body"); err == nil || result != nil {
		t.Fatalf("existing spill was trusted without validating content: result=%v err=%v", result, err)
	}
}

func TestStoreConcurrentRepeatedIDPublishesCompleteResults(t *testing.T) {
	dir := t.TempDir()
	const workers = 24
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := strings.Repeat(string(rune('A'+i%3)), 60000)
			r, err := Store(dir, "concurrent-provider-id", body)
			if err != nil {
				t.Errorf("Store: %v", err)
				return
			}
			got, err := os.ReadFile(r.Path)
			if err != nil || string(got) != body {
				t.Errorf("concurrent result exposed incomplete or unrelated content: same=%v bytes=%d err=%v", string(got) == body, len(got), err)
			}
		}(i)
	}
	wg.Wait()
}

func TestStoreRejectsExistingRecoverySymlink(t *testing.T) {
	dir := t.TempDir()
	r, err := Store(dir, "symlink-id", "expected body")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(r.Path); err != nil {
		t.Fatal(err)
	}
	outside, err := os.CreateTemp(t.TempDir(), "outside-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outside.WriteString("expected body"); err != nil {
		t.Fatal(err)
	}
	if err := outside.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside.Name(), r.Path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if result, err := Store(dir, "symlink-id", "expected body"); err == nil || result != nil {
		t.Fatalf("symlink was trusted as an owned recovery file: result=%v err=%v", result, err)
	}
}

func TestStoreBoundsProviderIDLabel(t *testing.T) {
	r, err := Store(t.TempDir(), strings.Repeat("unbounded-id/", 1000), "result")
	if err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(r.Path)
	if err != nil || string(full) != "result" {
		t.Fatalf("unbounded provider ID lost recovery: %q %v", full, err)
	}
}
