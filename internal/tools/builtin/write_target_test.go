package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func invocationContext(id string) context.Context {
	return tools.WithInvocationID(context.Background(), id)
}

func readForWriteTest(t *testing.T, gate *permission.Gate, state *ReadFileState, path string) {
	t.Helper()
	result, err := (Read{gate: gate, state: state}).Execute(context.Background(), map[string]any{"path": path, "limit": 5000})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("Read(%s) = %+v, %v", path, result, err)
	}
}

func TestWriteRejectsTargetSwapBetweenCanUseAndExecute(t *testing.T) {
	dir := t.TempDir()
	safe := filepath.Join(dir, "safe.txt")
	attacker := filepath.Join(dir, "attacker.txt")
	link := filepath.Join(dir, "target.txt")
	mustWritePinnedTestFile(t, safe, "safe\n")
	mustWritePinnedTestFile(t, attacker, "attacker\n")
	if err := os.Symlink(safe, link); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeBypassPermissions)
	state := NewReadFileState()
	readForWriteTest(t, gate, state, link)
	tool := Write{gate: gate, state: state, authorizer: newInvocationAuthorizer[approvedWriteTarget]()}
	ctx := invocationContext("write-before-open-swap")
	in := map[string]any{"path": link, "content": "replacement\n"}
	if got, _ := tool.CanUse(ctx, in); got != tools.PermissionAllow {
		t.Fatalf("CanUse = %v", got)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attacker, link); err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(ctx, in)
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("Execute = %+v, %v; want fail closed", result, err)
	}
	assertFileContent(t, safe, "safe\n")
	assertFileContent(t, attacker, "attacker\n")
}

func TestEditRejectsTargetSwapBetweenCanUseAndExecute(t *testing.T) {
	dir := t.TempDir()
	safe := filepath.Join(dir, "safe.txt")
	attacker := filepath.Join(dir, "attacker.txt")
	link := filepath.Join(dir, "target.txt")
	mustWritePinnedTestFile(t, safe, "alpha\n")
	mustWritePinnedTestFile(t, attacker, "alpha attacker\n")
	if err := os.Symlink(safe, link); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeBypassPermissions)
	state := NewReadFileState()
	readForWriteTest(t, gate, state, link)
	tool := Edit{gate: gate, state: state, authorizer: newInvocationAuthorizer[approvedExistingPath]()}
	ctx := invocationContext("edit-before-open-swap")
	in := map[string]any{"path": link, "old": "alpha", "new": "beta"}
	if got, _ := tool.CanUse(ctx, in); got != tools.PermissionAllow {
		t.Fatalf("CanUse = %v", got)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attacker, link); err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(ctx, in)
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("Execute = %+v, %v; want fail closed", result, err)
	}
	assertFileContent(t, safe, "alpha\n")
	assertFileContent(t, attacker, "alpha attacker\n")
}

func TestWriteApprovedExistingDeletionDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.txt")
	mustWritePinnedTestFile(t, path, "old\n")
	gate := permission.New(permission.ModeBypassPermissions)
	state := NewReadFileState()
	readForWriteTest(t, gate, state, path)
	tool := Write{gate: gate, state: state, authorizer: newInvocationAuthorizer[approvedWriteTarget]()}
	ctx := invocationContext("existing-deleted")
	in := map[string]any{"path": path, "content": "new\n"}
	tool.CanUse(ctx, in)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(ctx, in)
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("Execute = %+v, %v; want refusal", result, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("approved existing path was recreated: %v", err)
	}
}

func TestWriteAfterOpenSwapWritesOnlyApprovedInodeAndClearsState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	originalMoved := filepath.Join(dir, "approved-inode.txt")
	mustWritePinnedTestFile(t, path, "old\n")
	gate := permission.New(permission.ModeBypassPermissions)
	state := NewReadFileState()
	readForWriteTest(t, gate, state, path)
	tool := Write{
		gate: gate, state: state, authorizer: newInvocationAuthorizer[approvedWriteTarget](),
		afterOpen: func() {
			if err := os.Rename(path, originalMoved); err != nil {
				t.Fatal(err)
			}
			mustWritePinnedTestFile(t, path, "attacker\n")
		},
	}
	ctx := invocationContext("write-after-open-swap")
	in := map[string]any{"path": path, "content": "approved replacement\n"}
	tool.CanUse(ctx, in)
	result, err := tool.Execute(ctx, in)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("Execute = %+v, %v", result, err)
	}
	assertFileContent(t, originalMoved, "approved replacement\n")
	assertFileContent(t, path, "attacker\n")
	if _, ok := state.getFixed(path); ok {
		t.Fatal("post-write state remained bound to the replacement pathname")
	}
}

func TestEditBeforeCommitSwapWritesOnlyApprovedInodeAndClearsState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	originalMoved := filepath.Join(dir, "approved-inode.txt")
	mustWritePinnedTestFile(t, path, "alpha\n")
	gate := permission.New(permission.ModeBypassPermissions)
	state := NewReadFileState()
	readForWriteTest(t, gate, state, path)
	tool := Edit{
		gate: gate, state: state, authorizer: newInvocationAuthorizer[approvedExistingPath](),
		beforeCommit: func() {
			if err := os.Rename(path, originalMoved); err != nil {
				t.Fatal(err)
			}
			mustWritePinnedTestFile(t, path, "alpha attacker\n")
		},
	}
	ctx := invocationContext("edit-before-commit-swap")
	in := map[string]any{"path": path, "old": "alpha", "new": "beta"}
	tool.CanUse(ctx, in)
	result, err := tool.Execute(ctx, in)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("Execute = %+v, %v", result, err)
	}
	assertFileContent(t, originalMoved, "beta\n")
	assertFileContent(t, path, "alpha attacker\n")
	if _, ok := state.getFixed(path); ok {
		t.Fatal("post-edit state remained bound to the replacement pathname")
	}
}

func TestWriteApprovedNewLeafRacedInIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.txt")
	gate := permission.New(permission.ModeBypassPermissions)
	tool := Write{
		gate: gate, authorizer: newInvocationAuthorizer[approvedWriteTarget](),
		beforeLeaf: func() { mustWritePinnedTestFile(t, path, "raced\n") },
	}
	ctx := invocationContext("new-leaf-race")
	in := map[string]any{"path": path, "content": "model\n"}
	tool.CanUse(ctx, in)
	result, err := tool.Execute(ctx, in)
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("Execute = %+v, %v; want refusal", result, err)
	}
	assertFileContent(t, path, "raced\n")
}

func TestWriteRejectsCreatedParentSymlinkSwapWithoutEscaping(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	attacker := filepath.Join(dir, "attacker")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(attacker, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "missing", "leaf.txt")
	createdOriginal := filepath.Join(base, "created-original")
	gate := permission.New(permission.ModeBypassPermissions)
	tool := Write{
		gate: gate, authorizer: newInvocationAuthorizer[approvedWriteTarget](),
		afterDirectory: func(rel string) {
			if rel != "missing" {
				return
			}
			if err := os.Rename(filepath.Join(base, "missing"), createdOriginal); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(attacker, filepath.Join(base, "missing")); err != nil {
				t.Fatal(err)
			}
		},
	}
	ctx := invocationContext("new-parent-swap")
	in := map[string]any{"path": target, "content": "model\n"}
	tool.CanUse(ctx, in)
	result, err := tool.Execute(ctx, in)
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("Execute = %+v, %v; want parent-swap refusal", result, err)
	}
	if _, err := os.Lstat(filepath.Join(attacker, "leaf.txt")); !os.IsNotExist(err) {
		t.Fatalf("write escaped into attacker directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(createdOriginal, "leaf.txt")); !os.IsNotExist(err) {
		t.Fatalf("write continued after parent swap: %v", err)
	}
}

func TestDirectWriteRequiresImmediateAllowDecision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.txt")
	gate := permission.New(permission.ModeDefault) // Write => ASK
	result, err := (Write{gate: gate}).Execute(context.Background(), map[string]any{"path": path, "content": "blocked\n"})
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Output, "denied") {
		t.Fatalf("direct ASK Execute = %+v, %v; want fail closed", result, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("direct ASK created file: %v", err)
	}
	allowed := filepath.Join(t.TempDir(), "allowed.txt")
	result, err = (Write{gate: permission.New(permission.ModeBypassPermissions)}).Execute(context.Background(), map[string]any{"path": allowed, "content": "ok\n"})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("direct ALLOW Execute = %+v, %v", result, err)
	}
	assertFileContent(t, allowed, "ok\n")
}

func TestWriteCanReplaceExistingFileLargerThanContentLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large-existing.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxWriteContentBytes + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	tool := Write{
		gate:       permission.New(permission.ModeBypassPermissions),
		authorizer: newInvocationAuthorizer[approvedWriteTarget](),
	}
	ctx := invocationContext("large-existing-small-overwrite")
	in := map[string]any{"path": path, "content": "small replacement\n"}
	if got, reason := tool.CanUse(ctx, in); got != tools.PermissionAllow {
		t.Fatalf("CanUse = %v (%s)", got, reason)
	}
	result, err := tool.Execute(ctx, in)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("Execute = %+v, %v", result, err)
	}
	assertFileContent(t, path, "small replacement\n")
}

func TestDirectReadGrepAndEditRequireImmediateAllowDecision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	mustWritePinnedTestFile(t, path, "needle alpha\n")

	readGate := permission.New(permission.ModeDefault)
	readGate.AppendRules(permission.Rule{Tool: "Read", Match: path, Verb: permission.DecisionAsk, Source: "test:ask"})
	readResult, err := (Read{gate: readGate}).Execute(context.Background(), map[string]any{"path": path})
	if err != nil || readResult == nil || !readResult.IsError || strings.Contains(readResult.Output, "needle alpha") {
		t.Fatalf("direct Read ASK = %+v, %v; want refusal", readResult, err)
	}

	grepGate := permission.New(permission.ModeDefault)
	grepGate.AppendRules(permission.Rule{Tool: "Grep", Match: "needle", Verb: permission.DecisionAsk, Source: "test:ask"})
	grepResult, err := NewGrep(grepGate).Execute(context.Background(), map[string]any{"root": dir, "pattern": "needle"})
	if err != nil || grepResult == nil || !grepResult.IsError || strings.Contains(grepResult.Output, "needle alpha") {
		t.Fatalf("direct Grep ASK = %+v, %v; want refusal", grepResult, err)
	}

	editResult, err := (Edit{gate: permission.New(permission.ModeDefault)}).Execute(context.Background(), map[string]any{
		"path": path, "old": "alpha", "new": "beta",
	})
	if err != nil || editResult == nil || !editResult.IsError {
		t.Fatalf("direct Edit ASK = %+v, %v; want refusal", editResult, err)
	}
	assertFileContent(t, path, "needle alpha\n")
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
