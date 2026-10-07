package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type artifactProcessOutcome struct {
	Success  bool   `json:"success"`
	Conflict bool   `json:"conflict"`
	Expected int    `json:"expected"`
	Current  int    `json:"current"`
	Error    string `json:"error,omitempty"`
}

// The helpers execute the real Store in separate processes, as native Desktop
// text-turn workers do. Process-local storeLocks cannot serialize these calls.
func TestStoreConditionalUpdateAcrossProcesses(t *testing.T) {
	store := newTestStore(t)
	created, err := store.Create("session-a", "Original", "<p>original</p>")
	if err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(t.TempDir(), "start")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var commands []*exec.Cmd
	var outputs []*bytes.Buffer
	for _, content := range []string{"First process", "Second process"} {
		ready := gate + "-" + strings.ReplaceAll(content, " ", "-")
		cmd := artifactProcessCommand(ctx, store.Root(), "cas", created.ID, content, gate, ready)
		output := new(bytes.Buffer)
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
		outputs = append(outputs, output)
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		waitForArtifactProcessFile(t, ready)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var successes, conflicts int
	var winner string
	for i, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("worker failed: %v\n%s", err, outputs[i])
		}
		var outcome artifactProcessOutcome
		if err := json.Unmarshal(outputs[i].Bytes(), &outcome); err != nil {
			t.Fatalf("decode worker result: %v\n%s", err, outputs[i])
		}
		if outcome.Success {
			successes++
			winner = []string{"First process", "Second process"}[i]
		} else if outcome.Conflict && outcome.Expected == 1 && outcome.Current == 2 {
			conflicts++
		} else {
			t.Fatalf("worker must succeed or report a typed conflict: %+v", outcome)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want 1/1", successes, conflicts)
	}
	manifest, err := store.Get("session-a", created.ID)
	if err != nil || manifest.CurrentVersion != 2 || len(manifest.Versions) != 2 || manifest.Title != winner {
		t.Fatalf("concurrent processes changed the winning manifest: %+v, %v", manifest, err)
	}
	body, _, err := store.ReadVersion("session-a", created.ID, 2)
	if err != nil || !bytes.Contains(body, []byte(winner)) {
		t.Fatalf("winning immutable version was overwritten: %q, %v", body, err)
	}
	if _, _, err := store.ReadVersion("session-a", created.ID, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("conflict wrote version 3: %v", err)
	}
}

func TestStoreWritersWaitForCrossProcessTransactionLock(t *testing.T) {
	for _, operation := range []string{"create", "update", "cas", "delete", "delete-session"} {
		t.Run(operation, func(t *testing.T) {
			store := newTestStore(t)
			created, err := store.Create("session-a", "Original", "<p>original</p>")
			if err != nil {
				t.Fatal(err)
			}
			gate := filepath.Join(t.TempDir(), "release")
			ready := gate + "-ready"
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := artifactProcessCommand(ctx, store.Root(), "hold", created.ID, "", gate, ready)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			waitForArtifactProcessFile(t, ready)
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				close(started)
				var err error
				switch operation {
				case "create":
					_, err = store.Create("session-a", "New", "<p>new</p>")
				case "update":
					_, err = store.Update("session-a", created.ID, "", "<p>new</p>")
				case "cas":
					_, err = store.UpdateIfVersion("session-a", created.ID, "", "<p>new</p>", 1)
				case "delete":
					err = store.Delete("session-a", created.ID)
				case "delete-session":
					err = store.DeleteSession("session-a")
				}
				done <- err
			}()
			<-started
			select {
			case err := <-done:
				t.Fatalf("%s ran while another process held the transaction lock: %v", operation, err)
			case <-time.After(100 * time.Millisecond):
			}
			if err := os.WriteFile(gate, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("lock holder failed: %v\n%s", err, &output)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("writer did not resume after the process lock was released")
			}
		})
	}
}

func TestArtifactStoreSubprocessWorker(t *testing.T) {
	operation := os.Getenv("METIS_ARTIFACT_TEST_OPERATION")
	if operation == "" {
		return
	}
	store, err := NewStore(os.Getenv("METIS_ARTIFACT_TEST_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	if operation == "hold" {
		file, err := store.acquireWriteLock()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("METIS_ARTIFACT_TEST_READY"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		waitForArtifactProcessFile(t, os.Getenv("METIS_ARTIFACT_TEST_GATE"))
		releaseArtifactWriteLock(file)
		os.Exit(0)
	}
	if err := os.WriteFile(os.Getenv("METIS_ARTIFACT_TEST_READY"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForArtifactProcessFile(t, os.Getenv("METIS_ARTIFACT_TEST_GATE"))
	content := os.Getenv("METIS_ARTIFACT_TEST_CONTENT")
	_, err = store.UpdateIfVersion("session-a", os.Getenv("METIS_ARTIFACT_TEST_ID"), content, "<p>"+content+"</p>", 1)
	outcome := artifactProcessOutcome{Success: err == nil, Conflict: errors.Is(err, ErrVersionConflict)}
	var conflict *VersionConflictError
	if errors.As(err, &conflict) {
		outcome.Expected, outcome.Current = conflict.ExpectedVersion, conflict.CurrentVersion
	}
	if err != nil {
		outcome.Error = err.Error()
	}
	if err := json.NewEncoder(os.Stdout).Encode(outcome); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func artifactProcessCommand(ctx context.Context, root, operation, id, content, gate, ready string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestArtifactStoreSubprocessWorker$")
	cmd.Env = append(os.Environ(), "METIS_ARTIFACT_TEST_ROOT="+root, "METIS_ARTIFACT_TEST_OPERATION="+operation,
		"METIS_ARTIFACT_TEST_ID="+id, "METIS_ARTIFACT_TEST_CONTENT="+content,
		"METIS_ARTIFACT_TEST_GATE="+gate, "METIS_ARTIFACT_TEST_READY="+ready)
	return cmd
}

func waitForArtifactProcessFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for subprocess marker %s", filepath.Base(path))
}

func TestStoreWriteLockRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "public"} {
		t.Run(kind, func(t *testing.T) {
			store := newTestStore(t)
			lock := filepath.Join(store.Root(), artifactWriteLockFilename)
			switch kind {
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, lock); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "directory":
				if err := os.Mkdir(lock, 0o700); err != nil {
					t.Fatal(err)
				}
			case "public":
				if runtime.GOOS == "windows" {
					t.Skip("Windows file modes are synthetic")
				}
				if err := os.WriteFile(lock, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(lock, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.Create("session-a", "Rejected", "<p>rejected</p>"); !errors.Is(err, ErrUnsafeFile) {
				t.Fatalf("unsafe %s lock error = %v, want ErrUnsafeFile", kind, err)
			}
			items, err := store.List("session-a")
			if err != nil || len(items) != 0 {
				t.Fatalf("unsafe lock created artifact data: %+v, %v", items, err)
			}
		})
	}
}
