package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestSetupRuntimeReturnsFreshHeaderWriteError(t *testing.T) {
	isolateResumeRuntimeTest(t)
	const id = "blocked-fresh-header"
	oldWrite := writeFreshSessionHeader
	writeFreshSessionHeader = func(*session.Store, string, string, string, string, string, string) error {
		return errors.New("disk full")
	}
	t.Cleanup(func() { writeFreshSessionHeader = oldWrite })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := setupRuntime(ctx, &cliFlags{
		newSessionID: id,
		bare:         true,
		noAuthWizard: true,
	})
	if rt != nil {
		rt.Cleanup()
		t.Fatal("setupRuntime returned a runtime after header persistence failed")
	}
	if err == nil || !strings.Contains(err.Error(), "persist fresh session header") {
		t.Fatalf("setupRuntime error = %v", err)
	}
}

func TestPersistSessionTitleReturnsWriteError(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const id = "blocked-title"
	if err := os.Mkdir(filepath.Join(store.Dir, id+".jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := persistSessionTitle(store, id, "Desktop title"); err == nil || !strings.Contains(err.Error(), "persist title") {
		t.Fatalf("persistSessionTitle error = %v", err)
	}
}
