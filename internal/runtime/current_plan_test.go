package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCurrentPlanConcurrentWritesRemainWholeAndPrivate(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const writers = 24
	wanted := make(map[string]bool, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		body := fmt.Sprintf("# Plan %02d\n\n%s", i, strings.Repeat(fmt.Sprintf("body-%02d ", i), 100))
		wanted[strings.TrimSpace(body)] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := WriteCurrentPlan("concurrent", body); err != nil {
				t.Errorf("WriteCurrentPlan: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := ReadCurrentPlan("concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if !wanted[got] {
		t.Fatalf("concurrent write produced a torn or unknown plan: %q", got)
	}
	info, err := os.Stat(CurrentPlanPath("concurrent"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("current plan mode = %o, want private permissions", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(PlansDir())
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("plans directory mode = %o, want private permissions", dirInfo.Mode().Perm())
	}
	entries, err := os.ReadDir(PlansDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary plan file leaked after concurrent writes: %s", entry.Name())
		}
	}
}

func TestCurrentPlanPathCannotEscapePlansDirectory(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	path := CurrentPlanPath("../../outside")
	rel, err := filepath.Rel(PlansDir(), path)
	if err != nil {
		t.Fatal(err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("current plan escaped plans directory: %s", path)
	}
}
