package builtin

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	artifactstore "github.com/Ricardo-M-L/metis/internal/artifact"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func selectedArtifactTestContext(t *testing.T, owner string, manifest *artifactstore.Manifest) context.Context {
	t.Helper()
	prompt := fmt.Sprintf("%s\n\n```json\n{\"metis_artifact_annotation\":{\"artifactId\":%q,\"version\":%d,\"digest\":%q,\"targetId\":\"target-1\",\"instruction\":\"Make it smaller\",\"title\":\"Demo\",\"selection\":{\"id\":\"target-1\",\"tag\":\"h1\",\"selector\":\"html > body > h1\",\"text\":\"Demo\"}}}\n```", artifactstore.ArtifactEditPromptPrefix, manifest.ID, manifest.CurrentVersion, manifest.Versions[manifest.CurrentVersion-1].SHA256)
	ctx, err := artifactstore.BindEditReference(tasks.WithSessionID(context.Background(), owner), owner, prompt)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestArtifactSelectedEditEnforcesOmittedExpectedVersionAndOnlyOneUpdate(t *testing.T) {
	store, err := artifactstore.NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	tool := NewArtifact(nil, store)
	setArtifactTestSession(t, "another-global-session")
	manifest, err := store.Create("owner-a", "Demo", "<h1>Original</h1>")
	if err != nil {
		t.Fatal(err)
	}
	ctx := selectedArtifactTestContext(t, "owner-a", manifest)
	updated, err := tool.Execute(ctx, map[string]any{"action": "update", "id": manifest.ID, "html": "<h1>Smaller</h1>"})
	if err != nil || updated.IsError || updated.Presentation["version"] != 2 {
		t.Fatalf("selected edit without model expected_version: result=%+v err=%v", updated, err)
	}
	for _, retry := range []map[string]any{
		{"action": "update", "id": manifest.ID, "html": "<h1>Retry</h1>"},
		{"action": "update", "id": manifest.ID, "html": "<h1>Bypass</h1>", "expected_version": 2},
	} {
		result, err := tool.Execute(ctx, retry)
		if err != nil || !result.IsError || !strings.Contains(result.Output, "refresh and select again") || strings.Contains(result.Output, "revise from") {
			t.Fatalf("same selected request appended again: result=%+v err=%v", result, err)
		}
	}
	saved, err := store.Get("owner-a", manifest.ID)
	if err != nil || saved.CurrentVersion != 2 || len(saved.Versions) != 2 {
		t.Fatalf("failed retries changed the selected artifact: manifest=%+v err=%v", saved, err)
	}
}

func TestArtifactSelectedEditRejectsConcurrentRevisionWithoutModelExpectedVersion(t *testing.T) {
	store, err := artifactstore.NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := store.Create("owner-a", "Demo", "<h1>Original</h1>")
	if err != nil {
		t.Fatal(err)
	}
	ctx := selectedArtifactTestContext(t, "owner-a", manifest)
	if _, err := store.Update("owner-a", manifest.ID, "Newer", "<h1>Other newer edit</h1>"); err != nil {
		t.Fatal(err)
	}
	result, err := NewArtifact(nil, store).Execute(ctx, map[string]any{"action": "update", "id": manifest.ID, "title": "Stale", "html": "<h1>Stale selected edit</h1>"})
	if err != nil || !result.IsError || !strings.Contains(result.Output, "version changed") || !strings.Contains(result.Output, "refresh and select again") {
		t.Fatalf("stale selected base not enforced: result=%+v err=%v", result, err)
	}
	saved, err := store.Get("owner-a", manifest.ID)
	if err != nil || saved.Title != "Newer" || saved.CurrentVersion != 2 {
		t.Fatalf("stale edit changed newer snapshot: manifest=%+v err=%v", saved, err)
	}
}

func TestArtifactSelectedEditLeavesOtherArtifactsAndPlainRequestsCompatible(t *testing.T) {
	store, err := artifactstore.NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	selected, err := store.Create("owner-a", "Selected", "<h1>Selected</h1>")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create("owner-a", "Other", "<h1>Other</h1>")
	if err != nil {
		t.Fatal(err)
	}
	ctx := selectedArtifactTestContext(t, "owner-a", selected)
	tool := NewArtifact(permission.New(permission.ModeDefault), store)
	if decision, _ := tool.CanUse(ctx, map[string]any{"action": "update", "id": selected.ID}); decision != tools.PermissionAsk {
		t.Fatal("selected edit bypassed the original write approval")
	}
	for _, expected := range []any{nil, 2} {
		input := map[string]any{"action": "update", "id": other.ID, "html": "<h1>Ordinary edit</h1>"}
		if expected != nil {
			input["expected_version"] = expected
		}
		result, err := tool.Execute(ctx, input)
		if err != nil || result.IsError {
			t.Fatalf("unrelated artifact update changed: result=%+v err=%v", result, err)
		}
	}
	plain, err := artifactstore.BindEditReference(ctx, "owner-a", "Continue with an ordinary prompt")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		result, err := tool.Execute(plain, map[string]any{"action": "update", "id": selected.ID, "html": "<h1>Ordinary selected edit</h1>"})
		if err != nil || result.IsError {
			t.Fatalf("ordinary subsequent request inherited selected guard: result=%+v err=%v", result, err)
		}
	}
	foreign := tasks.WithSessionID(ctx, "owner-b")
	result, err := tool.Execute(foreign, map[string]any{"action": "update", "id": selected.ID, "html": "<h1>Foreign</h1>"})
	if err != nil || !result.IsError || !strings.Contains(result.Output, "session does not own artifact") || strings.Contains(result.Output, "current") {
		t.Fatalf("another session inherited a selected owner: result=%+v err=%v", result, err)
	}
}
